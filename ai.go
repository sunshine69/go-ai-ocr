package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type aiClient struct {
	httpClient *http.Client
	endpoint   string
	model      string
	apiKey     string
	prompt     string
	maxRetries int
}

func newAIClient(cfg config) *aiClient {
	return &aiClient{
		httpClient: &http.Client{Timeout: cfg.timeout},
		endpoint:   cfg.endpoint,
		model:      cfg.model,
		apiKey:     cfg.apiKey,
		prompt:     cfg.prompt,
		maxRetries: cfg.maxRetries,
	}
}

// --- OpenAI-compatible chat completions wire format (subset) ---

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

var extToMime = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".bmp":  "image/bmp",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// ExtractBatch reads imagePaths (in order — this is the whole point of
// batching: a handful of *consecutive* pages given to the model together
// in one request) and asks the model for one Markdown response covering
// all of them. There is no concurrency here: this is one blocking HTTP
// call, retried in place on failure.
func (c *aiClient) ExtractBatch(ctx context.Context, imagePaths []string) (string, error) {
	content := make([]contentPart, 0, len(imagePaths)+1)
	content = append(content, contentPart{Type: "text", Text: batchPrompt(c.prompt, len(imagePaths))})

	for _, p := range imagePaths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", p, err)
		}
		mimeType := extToMime[strings.ToLower(filepath.Ext(p))]
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		dataURI := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
		content = append(content, contentPart{Type: "image_url", ImageURL: &imageURL{URL: dataURI}})
	}

	reqBody := chatRequest{
		Model:       c.model,
		Temperature: 0,
		MaxTokens:   4096,
		Messages:    []chatMessage{{Role: "user", Content: content}},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		text, err := c.doRequest(ctx, payload)
		if err == nil {
			return text, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("after %d attempt(s): %w", c.maxRetries+1, lastErr)
}

// batchPrompt appends a short instruction covering how to lay out the
// response when more than one page image is attached, so a batch's output
// stays splittable/readable in the final concatenated document.
func batchPrompt(base string, nImages int) string {
	if nImages <= 1 {
		return base
	}
	return base + fmt.Sprintf("\n\nYou are given %d sequential page images from the same document, "+
		"in reading order. Produce output for all of them in one response. Before each page's content, "+
		"insert a line by itself: `<!-- page N -->` where N is that page's position in this batch (starting at 1). "+
		"Never merge two pages' content together without that marker between them.", nImages)
}

func (c *aiClient) doRequest(ctx context.Context, payload []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("endpoint returned %s: %s", resp.Status, truncate(string(body), 500))
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w (body: %s)", err, truncate(string(body), 500))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("endpoint error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("empty choices in response: %s", truncate(string(body), 500))
	}
	return parsed.Choices[0].Message.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
