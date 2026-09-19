package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Extract sends one page/image to the vision model and returns the
// structured text it produces.
func (c *aiClient) Extract(ctx context.Context, imgData []byte, mimeType string) (string, error) {
	dataURI := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(imgData)

	reqBody := chatRequest{
		Model:       c.model,
		Temperature: 0,
		MaxTokens:   4096,
		Messages: []chatMessage{
			{
				Role: "user",
				Content: []contentPart{
					{Type: "text", Text: c.prompt},
					{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
				},
			},
		},
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
