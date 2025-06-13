package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type ScrapedData struct {
	URL     string
	Title   string
	Links   []string
	Content string
	Error   error
}

type ScrapeJob struct {
	URL   string
	Depth int
}

type WebScraper struct {
	maxWorkers   int
	maxDepth     int
	delay        time.Duration
	visited      map[string]bool
	visitedMutex sync.RWMutex
	httpClient   *http.Client
	urlPattern   *regexp.Regexp
	titlePattern *regexp.Regexp
}

func NewWebScraper(maxWorkers, maxDepth int, delay time.Duration) *WebScraper {
	return &WebScraper{
		maxWorkers: maxWorkers,
		maxDepth:   maxDepth,
		delay:      delay,
		visited:    make(map[string]bool),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		urlPattern:   regexp.MustCompile(`href=["']([^"']+)["']`),
		titlePattern: regexp.MustCompile(`<title[^>]*>([^<]+)</title>`),
	}
}

func (ws *WebScraper) isVisited(url string) bool {
	ws.visitedMutex.RLock()
	defer ws.visitedMutex.RUnlock()
	return ws.visited[url]
}

func (ws *WebScraper) markVisited(url string) {
	ws.visitedMutex.Lock()
	defer ws.visitedMutex.Unlock()
	ws.visited[url] = true
}

func (ws *WebScraper) scrapeURL(ctx context.Context, targetURL string) ScrapedData {
	result := ScrapedData{URL: targetURL}

	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		result.Error = fmt.Errorf("creating request: %w", err)
		return result
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoScraper/1.0)")

	resp, err := ws.httpClient.Do(req)
	if err != nil {
		result.Error = fmt.Errorf("HTTP request failed: %w", err)
		return result
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
		return result
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Error = fmt.Errorf("reading response body: %w", err)
		return result
	}

	content := string(body)
	result.Content = content

	if matches := ws.titlePattern.FindStringSubmatch(content); len(matches) > 1 {
		result.Title = strings.TrimSpace(matches[1])
	}

	result.Links = ws.extractLinks(content, targetURL)

	return result
}

func (ws *WebScraper) extractLinks(content, baseURL string) []string {
	matches := ws.urlPattern.FindAllStringSubmatch(content, -1)
	var links []string
	seen := make(map[string]bool)

	baseURLParsed, err := url.Parse(baseURL)
	if err != nil {
		return links
	}

	for _, match := range matches {
		if len(match) > 1 {
			linkURL := match[1]

			if strings.HasPrefix(linkURL, "#") ||
				strings.HasPrefix(linkURL, "javascript:") ||
				strings.HasPrefix(linkURL, "mailto:") {
				continue
			}

			parsedURL, err := url.Parse(linkURL)
			if err != nil {
				continue
			}

			absoluteURL := baseURLParsed.ResolveReference(parsedURL).String()

			if (strings.HasPrefix(absoluteURL, "http://") ||
				strings.HasPrefix(absoluteURL, "https://")) && !seen[absoluteURL] {
				links = append(links, absoluteURL)
				seen[absoluteURL] = true
			}
		}
	}

	return links
}

func (ws *WebScraper) worker(ctx context.Context, id int, jobs <-chan ScrapeJob, results chan<- ScrapedData, newJobs chan<- ScrapeJob) {
	for {
		select {
		case <-ctx.Done():
			log.Printf("Worker %d shutting down", id)
			return
		case job, ok := <-jobs:
			if !ok {
				log.Printf("Worker %d: job channel closed", id)
				return
			}

			log.Printf("Worker %d processing: %s (depth %d)", id, job.URL, job.Depth)

			if ws.isVisited(job.URL) {
				continue
			}

			ws.markVisited(job.URL)

			if ws.delay > 0 {
				time.Sleep(ws.delay)
			}

			result := ws.scrapeURL(ctx, job.URL)

			select {
			case results <- result:
			case <-ctx.Done():
				return
			}

			if result.Error == nil && job.Depth < ws.maxDepth {
				for _, link := range result.Links {
					if !ws.isVisited(link) {
						newJob := ScrapeJob{
							URL:   link,
							Depth: job.Depth + 1,
						}

						select {
						case newJobs <- newJob:
						case <-ctx.Done():
							return
						default:
						}
					}
				}
			}
		}
	}
}

func (ws *WebScraper) Scrape(ctx context.Context, seedURLs []string) (<-chan ScrapedData, error) {
	if len(seedURLs) == 0 {
		return nil, fmt.Errorf("no seed URLs provided")
	}

	jobs := make(chan ScrapeJob, ws.maxWorkers*2)
	newJobs := make(chan ScrapeJob, ws.maxWorkers*2)
	results := make(chan ScrapedData, ws.maxWorkers)

	var wg sync.WaitGroup
	for i := 0; i < ws.maxWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ws.worker(ctx, id, jobs, results, newJobs)
		}(i)
	}

	go func() {
		defer close(jobs)

		for _, seedURL := range seedURLs {
			jobs <- ScrapeJob{URL: seedURL, Depth: 0}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case newJob, ok := <-newJobs:
				if !ok {
					return
				}
				select {
				case jobs <- newJob:
				case <-ctx.Done():
					return
				}
			case <-time.After(5 * time.Second):
				return
			}
		}
	}()

	resultChan := make(chan ScrapedData, 100)
	go func() {
		defer close(resultChan)
		for {
			select {
			case <-ctx.Done():
				return
			case result, ok := <-results:
				if !ok {
					return
				}
				select {
				case resultChan <- result:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
		close(newJobs)
	}()

	return resultChan, nil
}

func main() {
	scraper := NewWebScraper(
		5,
		2,
		500*time.Millisecond,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	seedURLs := []string{
		"https://example.com",
		"https://httpbin.org/html",
	}

	log.Printf("Starting scrape with %d workers, max depth %d", 5, 2)

	resultChan, err := scraper.Scrape(ctx, seedURLs)
	if err != nil {
		log.Fatal("Failed to start scraping:", err)
	}

	var successCount, errorCount int
	for result := range resultChan {
		if result.Error != nil {
			log.Printf("Error scraping %s: %v", result.URL, result.Error)
			errorCount++
		} else {
			log.Printf("Successfully scraped %s - Title: %s, Links found: %d",
				result.URL, result.Title, len(result.Links))
			successCount++
		}
	}

	log.Printf("Scraping completed. Success: %d, Errors: %d", successCount, errorCount)
}
