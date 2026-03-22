package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "golang.org/x/image/webp"
)

var (
	ImmichURL    string
	ImmichAPIKey string
	OllamaHost   string
	OllamaModel  string
	Prompt       string
	NumPredict   int
	VerboseMode  bool
)

// Ollama structs
type ChatRequest struct {
	Model    string                 `json:"model"`
	Messages []Message              `json:"messages"`
	Stream   bool                   `json:"stream"`
	Think    *bool                  `json:"think,omitempty"`
	Options  map[string]interface{} `json:"options"`
}

type Message struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type ChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

// Immich API structs
type SearchResponse struct {
	Assets struct {
		Items    []Asset `json:"items"`
		NextPage *string `json:"nextPage"`
	} `json:"assets"`
}

type Asset struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	ExifInfo struct {
		Description *string `json:"description"`
	} `json:"exifInfo"`
}

const defaultPrompt = "Name the 8 most important search keywords for this photo. Include any visible text. Format: keyword1, keyword2, keyword3"

func main() {
	godotenv.Load()

	flag.StringVar(&ImmichURL, "url", getEnv("IMMICH_URL", "http://localhost:2283"), "Immich base URL")
	flag.StringVar(&ImmichAPIKey, "key", getEnv("IMMICH_API_KEY", ""), "Immich API Key")
	flag.StringVar(&OllamaHost, "ollama", getEnv("OLLAMA_HOST", "http://localhost:11434"), "Ollama Server URL")
	flag.StringVar(&OllamaModel, "model", getEnv("OLLAMA_MODEL", "qwen3.5:9b"), "Ollama vision model")
	flag.StringVar(&Prompt, "prompt", getEnv("PROMPT", defaultPrompt), "VLM prompt for keyword generation")
	flag.IntVar(&NumPredict, "num-predict", 80, "Max tokens for Ollama response")
	flag.BoolVar(&VerboseMode, "verbose", false, "Print full descriptions")
	flag.Parse()

	if ImmichAPIKey == "" {
		log.Fatal("Immich API key required (-key or IMMICH_API_KEY)")
	}

	ImmichURL = strings.TrimRight(ImmichURL, "/")
	fmt.Printf("Immich: %s\nOllama: %s (model: %s)\n\n", ImmichURL, OllamaHost, OllamaModel)
	run()
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func run() {
	ollamaClient := &http.Client{Timeout: 0}
	totalProcessed := 0
	totalSkipped := 0
	page := 1

	for {
		fmt.Printf("Scanning page %d...\n", page)
		assets, nextPage, err := searchAssets(page)
		if err != nil {
			log.Fatalf("Search error: %v", err)
		}

		if len(assets) == 0 {
			break
		}

		for _, asset := range assets {
			// Search endpoint doesn't include exifInfo, fetch full asset
			full, err := getAsset(asset.ID)
			if err != nil {
				fmt.Printf("[?] %s ... SKIP (fetch: %v)\n", asset.ID, err)
				continue
			}
			desc := full.ExifInfo.Description
			if desc != nil && *desc != "" {
				totalSkipped++
				continue
			}

			totalProcessed++
			fmt.Printf("[%d] %s ... ", totalProcessed, asset.ID)

			imgBytes, err := downloadThumbnail(asset.ID)
			if err != nil {
				fmt.Printf("SKIP (download: %v)\n", err)
				continue
			}

			imgBytes, err = ensureJPEG(imgBytes)
			if err != nil {
				fmt.Printf("SKIP (convert: %v)\n", err)
				continue
			}

			b64 := base64.StdEncoding.EncodeToString(imgBytes)
			fmt.Print("analyzing ... ")

			keywords, err := generateDescription(ollamaClient, b64, OllamaModel)
			if err != nil {
				fmt.Printf("FAIL (ollama: %v)\n", err)
				continue
			}

			err = updateDescription(asset.ID, keywords)
			if err != nil {
				fmt.Printf("FAIL (api: %v)\n", err)
				continue
			}

			if VerboseMode {
				fmt.Printf("OK → %s\n", keywords)
			} else {
				fmt.Printf("OK (%d chars)\n", len(keywords))
			}
		}

		if nextPage == nil || *nextPage == "" {
			break
		}
		page++
	}

	fmt.Printf("\nDone! Processed %d images, skipped %d (already had descriptions).\n", totalProcessed, totalSkipped)
}

func searchAssets(page int) ([]Asset, *string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"type": "IMAGE",
		"page": page,
		"size": 100,
	})

	req, _ := http.NewRequest("POST", ImmichURL+"/api/search/metadata", bytes.NewBuffer(body))
	req.Header.Set("x-api-key", ImmichAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(b))
	}

	var result SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil, err
	}

	return result.Assets.Items, result.Assets.NextPage, nil
}

func getAsset(id string) (*Asset, error) {
	req, _ := http.NewRequest("GET", ImmichURL+"/api/assets/"+id, nil)
	req.Header.Set("x-api-key", ImmichAPIKey)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var asset Asset
	if err := json.NewDecoder(resp.Body).Decode(&asset); err != nil {
		return nil, err
	}
	return &asset, nil
}

func updateDescription(id, description string) error {
	body, _ := json.Marshal(map[string]string{"description": description})

	req, _ := http.NewRequest("PUT", ImmichURL+"/api/assets/"+id, bytes.NewBuffer(body))
	req.Header.Set("x-api-key", ImmichAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func downloadThumbnail(id string) ([]byte, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/assets/%s/thumbnail?size=preview", ImmichURL, id), nil)
	req.Header.Set("x-api-key", ImmichAPIKey)
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func generateDescription(client *http.Client, base64Image string, modelName string) (string, error) {
	thinkFalse := false
	payload := ChatRequest{
		Model:  modelName,
		Stream: false,
		Think:  &thinkFalse,
		Messages: []Message{
			{
				Role:    "user",
				Content: Prompt,
				Images:  []string{base64Image},
			},
		},
		Options: map[string]interface{}{
			"num_predict": NumPredict,
			"temperature": 0.1,
		},
	}

	jsonData, _ := json.Marshal(payload)

	resp, err := client.Post(OllamaHost+"/api/chat", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama status %d: %s", resp.StatusCode, string(b))
	}

	var response ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", err
	}

	return response.Message.Content, nil
}

func ensureJPEG(data []byte) ([]byte, error) {
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %v", err)
	}
	if format != "jpeg" {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return data, nil
}
