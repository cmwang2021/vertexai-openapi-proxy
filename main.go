package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
)

var logger *slog.Logger

func initSlogLogger() {
	var logLevel slog.Level
	logLevelStr := strings.ToLower(os.Getenv("LOG_LEVEL"))
	switch logLevelStr {
	case "debug": logLevel = slog.LevelDebug
	case "info": logLevel = slog.LevelInfo
	case "warn": logLevel = slog.LevelWarn
	case "error": logLevel = slog.LevelError
	default: logLevel = slog.LevelInfo
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if groups != nil { return a }
			if a.Key == slog.TimeKey { a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339Nano)) }
			if a.Key == slog.MessageKey { a.Key = "message" }
			if a.Key == slog.LevelKey { a = slog.String("severity", a.Value.Any().(slog.Level).String()) }
			return a
		},
	})
	logger = slog.New(handler)
}

var googleFindDefaultCredentials = google.FindDefaultCredentials
var (
	projectID string
	location  string
)

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

var (
	token      string
	tokenMutex sync.RWMutex
	expiry     time.Time
)

func getToken(ctx context.Context) (string, error) {
	tokenMutex.RLock()
	if time.Now().Before(expiry.Add(-time.Minute)) {
		tokenMutex.RUnlock()
		return token, nil
	}
	tokenMutex.RUnlock()
	tokenMutex.Lock()
	defer tokenMutex.Unlock()
	creds, err := googleFindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil { return "", err }
	tok, err := creds.TokenSource.Token()
	if err != nil { return "", err }
	token = tok.AccessToken
	expiry = tok.Expiry
	return token, nil
}

type transport struct {
	underlying http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	isModelAPI := strings.Contains(req.URL.Path, ":generateContent") || strings.Contains(req.URL.Path, ":predict")
	
	if isModelAPI && !strings.Contains(req.URL.Path, "locations/global") {
		regionalPath := req.URL.Path
		regionalHost := req.URL.Host

		req.URL.Host = "aiplatform.googleapis.com"
		req.Host = "aiplatform.googleapis.com"
		
		pathParts := strings.Split(regionalPath, "/models/")
		if len(pathParts) == 2 {
			modelID := pathParts[1]
			req.URL.Path = fmt.Sprintf("/v1/projects/%s/locations/global/publishers/google/models/%s", projectID, modelID)
		}

		logger.Info("transport: Attempting Global endpoint", "url", req.URL.String())
		resp, err := t.underlying.RoundTrip(req)
		
		if (err == nil && resp.StatusCode == 404) || err != nil {
			if err == nil { resp.Body.Close() }
			logger.Warn("transport: Global failed (404), falling back to Regional", "model", regionalPath)
			req.URL.Path = regionalPath
			req.URL.Host = regionalHost
			req.Host = regionalHost
			return t.underlying.RoundTrip(req)
		}
		return resp, err
	}
	return t.underlying.RoundTrip(req)
}

func makeProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			if strings.HasPrefix(req.URL.Path, "/v1/") {
				req.URL.Path = target.Path + strings.TrimPrefix(req.URL.Path, "/v1")
			} else {
				req.URL.Path = target.Path + req.URL.Path
			}
			if tok, err := getToken(req.Context()); err == nil {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		},
		Transport: &transport{underlying: http.DefaultTransport},
	}
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	modelIDs := []string{"gemini-3.5-flash", "gemini-3.1-flash-lite-preview", "gemini-2.5-pro", "gemini-2.5-flash"}
	envModels := os.Getenv("VERTEXAI_AVAILABLE_MODELS")
	if envModels != "" { modelIDs = strings.Split(envModels, ",") }
	
	currentTime := time.Now().Unix()
	data := make([]Model, len(modelIDs))
	for i, id := range modelIDs {
		data[i] = Model{ID: strings.TrimSpace(id), Object: "model", Created: currentTime, OwnedBy: "google"}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ModelList{Object: "list", Data: data})
}

func main() {
	initSlogLogger()
	location = os.Getenv("VERTEXAI_LOCATION")
	projectID = os.Getenv("VERTEXAI_PROJECT")
	if location == "" || projectID == "" { log.Fatal("Env vars missing") }

	proxyHost := fmt.Sprintf("%s-aiplatform.googleapis.com", location)
	baseURL := fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/endpoints/openapi", proxyHost, projectID, location)
	
	target, _ := url.Parse(baseURL)
	http.HandleFunc("/v1/models", handleModels)
	http.Handle("/v1/", makeProxy(target))
	port := os.Getenv("PORT")
	if port == "" { port = "8080" }
	logger.Info("proxy listening", "address", ":" + port)
	http.ListenAndServe(":" + port, nil)
}
