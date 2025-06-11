package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Configuration struct
type Config struct {
	FilePath       string
	ChunkSize      int
	APIKey         string
	Model          string
	Workers        int
	RateLimit      int
	RequestTimeout time.Duration
}

// EPUB parsing structures
type EPUBPackage struct {
	XMLName  xml.Name     `xml:"package"`
	Manifest EPUBManifest `xml:"manifest"`
	Spine    EPUBSpine    `xml:"spine"`
}

type EPUBManifest struct {
	Items []EPUBItem `xml:"item"`
}

type EPUBItem struct {
	ID        string `xml:"id,attr"`
	Href      string `xml:"href,attr"`
	MediaType string `xml:"media-type,attr"`
}

type EPUBSpine struct {
	ItemRefs []EPUBItemRef `xml:"itemref"`
}

type EPUBItemRef struct {
	IDRef string `xml:"idref,attr"`
}

type EPUBContainer struct {
	XMLName   xml.Name       `xml:"container"`
	RootFiles []EPUBRootFile `xml:"rootfiles>rootfile"`
}

type EPUBRootFile struct {
	FullPath  string `xml:"full-path,attr"`
	MediaType string `xml:"media-type,attr"`
}

// Claude API structures
type ClaudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ClaudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []ClaudeMessage `json:"messages"`
}

type ClaudeResponse struct {
	Content []struct {
		Text string `json:"text"`
		Type string `json:"type"`
	} `json:"content"`
	ID           string `json:"id"`
	Model        string `json:"model"`
	Role         string `json:"role"`
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
	Type         string `json:"type"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type ClaudeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ClaudeErrorResponse struct {
	Error ClaudeError `json:"error"`
}

// Processing structures
type TextChunk struct {
	ID      int
	Content string
	Summary string
	Error   error
}

type BookSummary struct {
	Title          string
	ChunkCount     int
	ChunkSummaries []string
	FinalSummary   string
	ProcessingTime time.Duration
}

// Progress tracking
type ProgressTracker struct {
	totalChunks     int
	completedChunks int
	failedChunks    int
	mu              sync.Mutex
	startTime       time.Time
}

func NewProgressTracker(totalChunks int) *ProgressTracker {
	return &ProgressTracker{
		totalChunks: totalChunks,
		startTime:   time.Now(),
	}
}

func (pt *ProgressTracker) UpdateProgress(success bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if success {
		pt.completedChunks++
	} else {
		pt.failedChunks++
	}

	processed := pt.completedChunks + pt.failedChunks
	percentage := float64(processed) / float64(pt.totalChunks) * 100
	elapsed := time.Since(pt.startTime)

	if processed > 0 {
		avgTimePerChunk := elapsed / time.Duration(processed)
		remaining := pt.totalChunks - processed
		eta := avgTimePerChunk * time.Duration(remaining)

		fmt.Fprintf(os.Stderr, "\rProgress: %d/%d chunks (%.1f%%) | Success: %d | Failed: %d | ETA: %v",
			processed, pt.totalChunks, percentage, pt.completedChunks, pt.failedChunks, eta.Round(time.Second))
	}

	if processed == pt.totalChunks {
		fmt.Fprintf(os.Stderr, "\n")
	}
}

// HTTP Client with proper configuration
func createHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// EPUB text extraction
func extractEPUBText(filePath string) (string, error) {
	// Open EPUB file as ZIP archive
	reader, err := zip.OpenReader(filePath)
	if err != nil {
		return "", fmt.Errorf("opening EPUB file: %w", err)
	}
	defer reader.Close()

	// Find container.xml to get OPF file location
	containerPath := "META-INF/container.xml"
	var containerFile *zip.File
	for _, file := range reader.File {
		if file.Name == containerPath {
			containerFile = file
			break
		}
	}

	if containerFile == nil {
		return "", errors.New("container.xml not found in EPUB")
	}

	// Parse container.xml
	containerReader, err := containerFile.Open()
	if err != nil {
		return "", fmt.Errorf("opening container.xml: %w", err)
	}
	defer containerReader.Close()

	var container EPUBContainer
	if err := xml.NewDecoder(containerReader).Decode(&container); err != nil {
		return "", fmt.Errorf("parsing container.xml: %w", err)
	}

	if len(container.RootFiles) == 0 {
		return "", errors.New("no rootfile found in container.xml")
	}

	// Get OPF file path
	opfPath := container.RootFiles[0].FullPath
	var opfFile *zip.File
	for _, file := range reader.File {
		if file.Name == opfPath {
			opfFile = file
			break
		}
	}

	if opfFile == nil {
		return "", fmt.Errorf("OPF file not found: %s", opfPath)
	}

	// Parse OPF file
	opfReader, err := opfFile.Open()
	if err != nil {
		return "", fmt.Errorf("opening OPF file: %w", err)
	}
	defer opfReader.Close()

	var epubPackage EPUBPackage
	if err := xml.NewDecoder(opfReader).Decode(&epubPackage); err != nil {
		return "", fmt.Errorf("parsing OPF file: %w", err)
	}

	// Build map of manifest items
	manifestMap := make(map[string]EPUBItem)
	for _, item := range epubPackage.Manifest.Items {
		manifestMap[item.ID] = item
	}

	// Get ordered content files from spine
	var contentFiles []string
	opfDir := filepath.Dir(opfPath)

	for _, itemRef := range epubPackage.Spine.ItemRefs {
		if item, exists := manifestMap[itemRef.IDRef]; exists {
			if item.MediaType == "application/xhtml+xml" || item.MediaType == "text/html" {
				fullPath := filepath.Join(opfDir, item.Href)
				// Normalize path separators for zip file
				fullPath = strings.ReplaceAll(fullPath, "\\", "/")
				contentFiles = append(contentFiles, fullPath)
			}
		}
	}

	if len(contentFiles) == 0 {
		return "", errors.New("no content files found in EPUB")
	}

	// Extract text from content files
	var textBuilder strings.Builder
	htmlTagRegex := regexp.MustCompile(`<[^>]*>`)
	entityRegex := regexp.MustCompile(`&[a-zA-Z][a-zA-Z0-9]*;|&#[0-9]+;|&#x[0-9a-fA-F]+;`)
	whitespaceRegex := regexp.MustCompile(`\s+`)

	for _, contentPath := range contentFiles {
		var contentFile *zip.File
		for _, file := range reader.File {
			if file.Name == contentPath {
				contentFile = file
				break
			}
		}

		if contentFile == nil {
			continue // Skip missing files
		}

		contentReader, err := contentFile.Open()
		if err != nil {
			continue // Skip files that can't be opened
		}

		content, err := io.ReadAll(contentReader)
		contentReader.Close()
		if err != nil {
			continue // Skip files that can't be read
		}

		// Extract text from HTML/XHTML
		text := string(content)

		// Remove HTML tags
		text = htmlTagRegex.ReplaceAllString(text, " ")

		// Decode common HTML entities
		text = strings.ReplaceAll(text, "&lt;", "<")
		text = strings.ReplaceAll(text, "&gt;", ">")
		text = strings.ReplaceAll(text, "&amp;", "&")
		text = strings.ReplaceAll(text, "&quot;", "\"")
		text = strings.ReplaceAll(text, "&apos;", "'")
		text = strings.ReplaceAll(text, "&nbsp;", " ")
		text = strings.ReplaceAll(text, "&#8212;", "—")
		text = strings.ReplaceAll(text, "&#8211;", "–")
		text = strings.ReplaceAll(text, "&#8217;", "'")
		text = strings.ReplaceAll(text, "&#8220;", "\"")
		text = strings.ReplaceAll(text, "&#8221;", "\"")

		// Remove remaining entities
		text = entityRegex.ReplaceAllString(text, " ")

		// Normalize whitespace
		text = whitespaceRegex.ReplaceAllString(text, " ")
		text = strings.TrimSpace(text)

		if len(text) > 0 {
			textBuilder.WriteString(text)
			textBuilder.WriteString(" ")
		}
	}

	extractedText := strings.TrimSpace(textBuilder.String())
	if len(extractedText) == 0 {
		return "", errors.New("no text content extracted from EPUB")
	}

	return extractedText, nil
}

// Advanced text chunking with word boundary preservation
func chunkTextWithWordBoundaries(text string, maxChunkSize int) []string {
	if len(text) <= maxChunkSize {
		return []string{text}
	}

	// Clean and normalize text
	text = strings.TrimSpace(text)
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")

	// Hierarchical separators for splitting
	separators := []string{"\n\n", "\n", ". ", "! ", "? ", "; ", ", ", " "}

	return recursiveChunk(text, maxChunkSize, separators, 0)
}

func recursiveChunk(text string, maxSize int, separators []string, sepIndex int) []string {
	if len(text) <= maxSize {
		return []string{text}
	}

	if sepIndex >= len(separators) {
		// Fallback: split at maxSize maintaining word boundary
		if maxSize < len(text) {
			// Find last space before maxSize
			cutPoint := maxSize
			for cutPoint > 0 && text[cutPoint] != ' ' {
				cutPoint--
			}
			if cutPoint == 0 {
				cutPoint = maxSize // No space found, hard cut
			}
			return append([]string{text[:cutPoint]}, recursiveChunk(text[cutPoint:], maxSize, separators, sepIndex)...)
		}
		return []string{text}
	}

	separator := separators[sepIndex]
	parts := strings.Split(text, separator)

	var result []string
	var currentChunk strings.Builder

	for _, part := range parts {
		testContent := part
		if currentChunk.Len() > 0 {
			testContent = currentChunk.String() + separator + part
		}

		if len(testContent) <= maxSize {
			if currentChunk.Len() > 0 {
				currentChunk.WriteString(separator)
			}
			currentChunk.WriteString(part)
		} else {
			// Current chunk is full, save it and start new one
			if currentChunk.Len() > 0 {
				result = append(result, strings.TrimSpace(currentChunk.String()))
				currentChunk.Reset()
			}

			// If this part alone is too big, recurse with next separator
			if len(part) > maxSize {
				subChunks := recursiveChunk(part, maxSize, separators, sepIndex+1)
				result = append(result, subChunks...)
			} else {
				currentChunk.WriteString(part)
			}
		}
	}

	// Add final chunk if not empty
	if currentChunk.Len() > 0 {
		result = append(result, strings.TrimSpace(currentChunk.String()))
	}

	return result
}

// Claude API client
type ClaudeClient struct {
	httpClient  *http.Client
	apiKey      string
	model       string
	rateLimiter *rate.Limiter
}

func NewClaudeClient(apiKey, model string, rateLimit int) *ClaudeClient {
	return &ClaudeClient{
		httpClient:  createHTTPClient(),
		apiKey:      apiKey,
		model:       model,
		rateLimiter: rate.NewLimiter(rate.Limit(rateLimit), rateLimit*2), // Burst = 2x rate
	}
}

func (c *ClaudeClient) SummarizeChunk(ctx context.Context, chunkContent string, chunkID int) (string, error) {
	// Wait for rate limit
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("rate limit wait: %w", err)
	}

	prompt := fmt.Sprintf(`Analyze this text chunk (chunk %d) and provide a comprehensive summary with the following structure:

**Content Summary:**
- [Bullet points describing what the author discusses in this section]

**Critical Analysis:**
- [Identify any potential bias, logical flaws, or questionable claims]
- [Note the strength of evidence presented]

**Key Points:**
- [Main themes, arguments, or information presented]

Text to analyze:
%s`, chunkID, chunkContent)

	request := ClaudeRequest{
		Model:     c.model,
		MaxTokens: 1000,
		Messages: []ClaudeMessage{
			{
				Role:    "user",
				Content: prompt,
			},
		},
	}

	reqBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errorResp ClaudeErrorResponse
		if json.Unmarshal(respBody, &errorResp) == nil {
			return "", fmt.Errorf("Claude API error (%d): %s", resp.StatusCode, errorResp.Error.Message)
		}
		return "", fmt.Errorf("Claude API error (%d): %s", resp.StatusCode, string(respBody))
	}

	var response ClaudeResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return "", fmt.Errorf("unmarshaling response: %w", err)
	}

	if len(response.Content) == 0 {
		return "", errors.New("empty response from Claude API")
	}

	return response.Content[0].Text, nil
}

func (c *ClaudeClient) GenerateFinalSummary(ctx context.Context, chunkSummaries []string, title string) (string, error) {
	if err := c.rateLimiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("rate limit wait: %w", err)
	}

	summariesText := strings.Join(chunkSummaries, "\n\n---\n\n")

	prompt := fmt.Sprintf(`Based on the following chunk summaries from the book "%s", create a comprehensive final summary with this structure:

**Overall Summary:**
[2-3 paragraphs capturing the main themes, arguments, and conclusions of the entire book]

**Key Insights:**
- [Main insights and takeaways from the book]
- [Important concepts or frameworks presented]

**Critical Assessment:**
- [Overall assessment of the author's arguments and evidence]
- [Notable biases, strengths, or weaknesses in the work]

**Conclusion:**
[Final assessment of the book's value and main contributions]

Chunk summaries to synthesize:
%s`, title, summariesText)

	request := ClaudeRequest{
		Model:     c.model,
		MaxTokens: 2000,
		Messages: []ClaudeMessage{
			{
				Role:    "user",
				Content: prompt,
			},
		},
	}

	reqBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errorResp ClaudeErrorResponse
		if json.Unmarshal(respBody, &errorResp) == nil {
			return "", fmt.Errorf("Claude API error (%d): %s", resp.StatusCode, errorResp.Error.Message)
		}
		return "", fmt.Errorf("Claude API error (%d): %s", resp.StatusCode, string(respBody))
	}

	var response ClaudeResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return "", fmt.Errorf("unmarshaling response: %w", err)
	}

	if len(response.Content) == 0 {
		return "", errors.New("empty response from Claude API")
	}

	return response.Content[0].Text, nil
}

// Worker pool for concurrent processing with progress tracking and streaming output
func processChunksWithWorkers(ctx context.Context, chunks []string, client *ClaudeClient, numWorkers int, progressTracker *ProgressTracker) ([]string, error) {
	jobs := make(chan TextChunk, len(chunks))
	results := make(chan TextChunk, len(chunks))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for job := range jobs {
				select {
				case <-ctx.Done():
					job.Error = ctx.Err()
					results <- job
					progressTracker.UpdateProgress(false)
					return
				default:
					summary, err := client.SummarizeChunk(ctx, job.Content, job.ID)
					job.Summary = summary
					job.Error = err
					results <- job
					progressTracker.UpdateProgress(err == nil)
				}
			}
		}(i)
	}

	// Send jobs
	go func() {
		defer close(jobs)
		for i, chunk := range chunks {
			select {
			case <-ctx.Done():
				return
			case jobs <- TextChunk{ID: i + 1, Content: chunk}:
			}
		}
	}()

	// Close results when all workers are done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results and stream successful summaries immediately
	processedChunks := make([]TextChunk, 0, len(chunks))
	var summaries []string
	var errorCount int

	for result := range results {
		processedChunks = append(processedChunks, result)

		if result.Error != nil {
			fmt.Fprintf(os.Stderr, "Error processing chunk %d: %v\n", result.ID, result.Error)
			errorCount++
		} else {
			// Stream the summary immediately to stdout
			fmt.Printf("CHUNK %d SUMMARY:\n", result.ID)
			fmt.Println(result.Summary)
			fmt.Println("--------------------")
			summaries = append(summaries, result.Summary)
		}
	}

	if errorCount > 0 {
		fmt.Fprintf(os.Stderr, "Warning: %d chunks failed to process\n", errorCount)
	}

	return summaries, nil
}

// Main processing function
func ProcessEPUBBook(config Config) (*BookSummary, error) {
	startTime := time.Now()

	fmt.Fprintf(os.Stderr, "Extracting text from EPUB file: %s\n", config.FilePath)

	// Extract text from EPUB file
	text, err := extractEPUBText(config.FilePath)
	if err != nil {
		return nil, fmt.Errorf("extracting EPUB text: %w", err)
	}

	if len(text) == 0 {
		return nil, errors.New("extracted text is empty")
	}

	fmt.Fprintf(os.Stderr, "Extracted %d characters. Chunking text...\n", len(text))

	// Chunk the text
	chunks := chunkTextWithWordBoundaries(text, config.ChunkSize)
	fmt.Fprintf(os.Stderr, "Created %d chunks. Processing with Claude API using %d workers...\n", len(chunks), config.Workers)

	// Print book header to stdout
	title := filepath.Base(config.FilePath)
	fmt.Printf("Book: %s\n", title)
	fmt.Printf("Total chunks to process: %d\n\n", len(chunks))

	// Create progress tracker
	progressTracker := NewProgressTracker(len(chunks))

	// Create Claude client
	client := NewClaudeClient(config.APIKey, config.Model, config.RateLimit)

	// Process chunks with workers and stream results
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	summaries, err := processChunksWithWorkers(ctx, chunks, client, config.Workers, progressTracker)
	if err != nil {
		return nil, fmt.Errorf("processing chunks: %w", err)
	}

	if len(summaries) == 0 {
		return nil, errors.New("no chunks were successfully processed")
	}

	fmt.Fprintf(os.Stderr, "Generating final summary from %d chunk summaries...\n", len(summaries))

	// Generate final summary
	finalSummary, err := client.GenerateFinalSummary(ctx, summaries, title)
	if err != nil {
		return nil, fmt.Errorf("generating final summary: %w", err)
	}

	// Print final summary to stdout
	fmt.Println("FINAL BOOK SUMMARY:")
	fmt.Println(finalSummary)
	fmt.Println("--------------------")

	fmt.Fprintf(os.Stderr, "Processing completed in: %v\n", time.Since(startTime))

	return &BookSummary{
		Title:          title,
		ChunkCount:     len(chunks),
		ChunkSummaries: summaries,
		FinalSummary:   finalSummary,
		ProcessingTime: time.Since(startTime),
	}, nil
}

// Output formatting - now outputs to stdout
func printResults(summary *BookSummary) {
	fmt.Printf("Book: %s\n", summary.Title)
	fmt.Printf("Total chunks processed: %d\n\n", len(summary.ChunkSummaries))

	// Print chunk summaries
	for i, chunkSummary := range summary.ChunkSummaries {
		fmt.Printf("CHUNK %d SUMMARY:\n", i+1)
		fmt.Println(chunkSummary)
		fmt.Println("--------------------")
	}

	// Print final summary
	fmt.Println("FINAL BOOK SUMMARY:")
	fmt.Println(summary.FinalSummary)
	fmt.Println("--------------------")
}

// Command line argument parsing
func parseArgs() (Config, error) {
	if len(os.Args) < 2 {
		return Config{}, errors.New("usage: epub-summarizer <file.epub> [options]")
	}

	config := Config{
		FilePath:       os.Args[1],
		ChunkSize:      15000,                     // Default chunk size in characters (3000 words * 5 chars/word)
		Model:          "claude-3-haiku-20240307", // Fast and cost-effective
		Workers:        3,                         // Conservative default for rate limiting
		RateLimit:      5,                         // Requests per second
		RequestTimeout: 60 * time.Second,
	}

	// Get API key from environment
	config.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	if config.APIKey == "" {
		return Config{}, errors.New("ANTHROPIC_API_KEY environment variable not set")
	}

	// Parse optional arguments
	for i := 2; i < len(os.Args); i++ {
		arg := os.Args[i]
		if strings.HasPrefix(arg, "--chunk-size=") {
			if size, err := strconv.Atoi(strings.TrimPrefix(arg, "--chunk-size=")); err == nil && size > 0 {
				config.ChunkSize = size
			}
		} else if strings.HasPrefix(arg, "--workers=") {
			if workers, err := strconv.Atoi(strings.TrimPrefix(arg, "--workers=")); err == nil && workers > 0 {
				config.Workers = workers
			}
		} else if strings.HasPrefix(arg, "--rate-limit=") {
			if rate, err := strconv.Atoi(strings.TrimPrefix(arg, "--rate-limit=")); err == nil && rate > 0 {
				config.RateLimit = rate
			}
		}
	}

	return config, nil
}

func main() {
	config, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		fmt.Fprintln(os.Stderr, "Usage: epub-summarizer <file.epub> [options]")
		fmt.Fprintln(os.Stderr, "Options:")
		fmt.Fprintln(os.Stderr, "  --chunk-size=N    Chunk size in characters (default: 15000, ~3000 words)")
		fmt.Fprintln(os.Stderr, "  --workers=N       Number of concurrent workers (default: 3)")
		fmt.Fprintln(os.Stderr, "  --rate-limit=N    API requests per second (default: 5)")
		fmt.Fprintln(os.Stderr, "\nEnvironment variables:")
		fmt.Fprintln(os.Stderr, "  ANTHROPIC_API_KEY  Your Claude API key (required)")
		fmt.Fprintln(os.Stderr, "\nExample:")
		fmt.Fprintln(os.Stderr, "  export ANTHROPIC_API_KEY=your_api_key_here")
		fmt.Fprintln(os.Stderr, "  epub-summarizer book.epub --chunk-size=12000 --workers=5")
		os.Exit(1)
	}

	// Validate file exists
	if _, err := os.Stat(config.FilePath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: File %s does not exist\n", config.FilePath)
		os.Exit(1)
	}

	// Process the book (summaries are streamed to stdout during processing)
	_, err = ProcessEPUBBook(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error processing book: %v\n", err)
		os.Exit(1)
	}
}
