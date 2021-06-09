package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
	"golang.org/x/net/publicsuffix"
)

func init() {
	flag.Usage = func() {
		h := []string{
			"Request URLs using headless Chrome, storing the results",
			"",
			"Usage:",
			"  page-fetch [options] < urls.txt",
			"",
			"Options:",
			"  -c, --concurrency <int>   Concurrency Level (default 2)",
			"  -e, --exclude <string>    Do not save responses matching the provided string (can be specified multiple times)",
			"  -i, --include <string>    Only save requests matching the provided string (can be specified multiple times)",
			"  -j, --javascript <string> JavaScript to run on each page",
			"  -o, --output <string>     Output directory name (default 'out')",
			"  -w, --overwrite           Overwrite output files when they already exist",
			"      --no-third-party      Do not save responses to requests on third-party domains",
			"      --third-party         Only save responses to requests on third-party domains",
			"",
		}

		fmt.Fprint(os.Stderr, strings.Join(h, "\n"))
	}
}

type options struct {
	includes       listArg
	excludes       listArg
	thirdPartyOnly bool
	noThirdParty   bool
	overwrite      bool
	output         string
	concurrency    int
	js             string
}

func main() {

	opts := options{}

	flag.Var(&opts.includes, "include", "")
	flag.Var(&opts.includes, "i", "")

	flag.Var(&opts.excludes, "exclude", "")
	flag.Var(&opts.excludes, "e", "")

	flag.BoolVar(&opts.thirdPartyOnly, "third-party", false, "")
	flag.BoolVar(&opts.noThirdParty, "no-third-party", false, "")

	flag.BoolVar(&opts.overwrite, "overwrite", false, "")
	flag.BoolVar(&opts.overwrite, "w", false, "")

	flag.StringVar(&opts.output, "output", "out", "")
	flag.StringVar(&opts.output, "o", "out", "")

	flag.IntVar(&opts.concurrency, "concurrency", 2, "")
	flag.IntVar(&opts.concurrency, "c", 2, "")

	flag.StringVar(&opts.js, "j", "", "")
	flag.StringVar(&opts.js, "javascript", "", "")

	flag.Parse()

	if opts.thirdPartyOnly && opts.noThirdParty {
		fmt.Fprintln(os.Stderr, "you cannot specify --third-party *and* --no-third-party")
		return
	}

	copts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("ignore-certificate-errors", true),
	)
	ectx, ecancel := chromedp.NewExecAllocator(context.Background(), copts...)
	defer ecancel()

	pctx, pcancel := chromedp.NewContext(ectx)
	defer pcancel()

	// start the browser to ensure we end up making new tabs in an
	// existing browser instead of making a new browser each time.
	// see: https://godoc.org/github.com/chromedp/chromedp#NewContext
	if err := chromedp.Run(pctx); err != nil {
		fmt.Fprintf(os.Stderr, "error starting browser: %s\n", err)
		return
	}

	sc := bufio.NewScanner(os.Stdin)

	var wg sync.WaitGroup
	jobs := make(chan string)

	for i := 0; i < opts.concurrency; i++ {
		wg.Add(1)
		go func() {
			for requestURL := range jobs {

				ctx, cancel := context.WithTimeout(pctx, time.Second*10)
				ctx, _ = chromedp.NewContext(ctx)

				// we want to intercept all requests, so we add a listener here
				chromedp.ListenTarget(ctx, makeListener(ctx, requestURL, opts))

				// default to evaluating "false" to avoid errant errors
				jsCode := opts.js
				if jsCode == "" {
					jsCode = "false"
				}

				var jsOutput interface{}
				err := chromedp.Run(
					ctx,
					fetch.Enable().WithPatterns([]*fetch.RequestPattern{{RequestStage: fetch.RequestStageResponse}}),
					chromedp.Navigate(requestURL),
					chromedp.EvaluateAsDevTools(jsCode, &jsOutput),
				)

				if opts.js != "" {
					fmt.Printf("JS (%s): %v\n", requestURL, jsOutput)
				}

				if err != nil {
					fmt.Fprintf(os.Stderr, "run error: %s\n", err)
				}

				cancel()
			}
			wg.Done()
		}()
	}
	for sc.Scan() {
		jobs <- sc.Text()
	}
	close(jobs)

	wg.Wait()
}

func saveResponse(requestURL string, data []byte, output string, overwrite bool) (string, error) {

	path, err := makeFilepath(output, requestURL)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(path)
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return "", err
	}

	i := 1
	for !overwrite {
		// should probably do something like get all the files
		// that start with the path, sort them, pick a number
		// one higher than the highest, or something like that
		// but unless there's thousands of duplicate file this
		// will work just fine
		if _, err := os.Stat(path); err != nil {
			break
		}

		path = fmt.Sprintf("%s.%d", strings.TrimRight(path, ".1234567890"), i)
		i++
	}

	return path, ioutil.WriteFile(path, data, 0644)

}

func makeFilepath(prefix, requestURL string) (string, error) {
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", err
	}

	requestPath := u.EscapedPath()
	if requestPath == "/" {
		requestPath = "/index"
	}

	savePath := fmt.Sprintf("%s/%s%s", prefix, u.Hostname(), requestPath)

	re := regexp.MustCompile("[^a-zA-Z0-9_.%/-]")
	savePath = re.ReplaceAllString(savePath, "-")

	// remove multiple dashes in a row
	re = regexp.MustCompile("-+")
	savePath = re.ReplaceAllString(savePath, "-")

	// remove multiple slashes in a row
	re = regexp.MustCompile("/+")
	savePath = re.ReplaceAllString(savePath, "/")

	// we shouldn't see any, but remove any double-dots just in case
	re = regexp.MustCompile("\\.\\.")
	savePath = re.ReplaceAllString(savePath, "-")

	savePath = strings.TrimSuffix(savePath, "/")

	return savePath, nil

}

func saveMeta(path string, parentURL string, ev *fetch.EventRequestPaused) error {

	b := &bytes.Buffer{}

	fmt.Fprintf(b, "url: %s\n", ev.Request.URL)
	fmt.Fprintf(b, "parent: %s\n", parentURL)
	fmt.Fprintf(b, "method: %s\n", ev.Request.Method)
	fmt.Fprintf(b, "type: %s\n", ev.ResourceType)

	b.WriteRune('\n')

	for k, v := range ev.Request.Headers {
		fmt.Fprintf(b, "> %s: %s\n", k, v)
	}

	if ev.Request.PostData != "" {
		fmt.Fprintf(b, "\n%s\n", ev.Request.PostData)
	}

	b.WriteRune('\n')

	for _, h := range ev.ResponseHeaders {
		fmt.Fprintf(b, "< %s: %s\n", h.Name, h.Value)
	}

	return ioutil.WriteFile(path, b.Bytes(), 0644)
}

func shouldSave(ev *fetch.EventRequestPaused, requestURL string, opts options) bool {

	contentType := "unknown"
	for _, h := range ev.ResponseHeaders {
		if strings.ToLower(h.Name) == "content-type" {
			contentType = strings.ToLower(h.Value)
		}
	}

	for _, i := range opts.includes {
		if strings.Contains(contentType, strings.ToLower(i)) {
			break
		}
		return false
	}

	for _, e := range opts.excludes {
		if strings.Contains(contentType, strings.ToLower(e)) {
			return false
		}
	}

	var domain string
	if u, err := url.Parse(requestURL); err == nil {
		domain = u.Hostname()
	}

	var subRequestDomain string
	if u, err := url.Parse(ev.Request.URL); err == nil {
		subRequestDomain = u.Hostname()
	}

	if opts.thirdPartyOnly {
		return isThirdParty(domain, subRequestDomain)
	}

	// you might be thinking "wait, what if opts.thirdPartyOnly and
	// opts.noThirdParty are both true?!". We check in main() that
	// is not the case so we should be all good here (:
	if opts.noThirdParty {
		return !isThirdParty(domain, subRequestDomain)
	}

	return true
}

func makeListener(ctx context.Context, requestURL string, opts options) func(interface{}) {

	return func(ev interface{}) {
		if ev, ok := ev.(*fetch.EventRequestPaused); ok {

			go func() {

				contentType := "unknown"
				for _, h := range ev.ResponseHeaders {
					if strings.ToLower(h.Name) == "content-type" {
						contentType = strings.ToLower(h.Value)
					}
				}

				if !shouldSave(ev, requestURL, opts) {
					err := chromedp.Run(ctx, fetch.ContinueRequest(ev.RequestID))
					if err != nil {
						fmt.Fprintf(os.Stderr, "continue request err on unmatched request: %s\n", err)
					}
					return
				}

				err := chromedp.Run(
					ctx,
					chromedp.ActionFunc(func(ctx context.Context) error {
						data, err := fetch.GetResponseBody(ev.RequestID).Do(ctx)
						if err != nil {
							// this function always has to return a nil error
							// otherwise the ContinueRequest does not run
							return nil
						}

						path, err := saveResponse(ev.Request.URL, data, opts.output, opts.overwrite)
						if err != nil {
							fmt.Fprintf(os.Stderr, "failed to save response data for %s: %s\n", ev.Request.URL, err)
							return nil
						}

						// save the headers etc in a separate file
						err = saveMeta(path+".meta", requestURL, ev)
						if err != nil {
							fmt.Fprintf(os.Stderr, "failed to save response meta data for %s: %s\n", ev.Request.URL, err)
							return nil
						}

						// Log the request
						fmt.Printf("%s %s %d %s\n", ev.Request.Method, ev.Request.URL, ev.ResponseStatusCode, contentType)

						return nil
					}),
					fetch.ContinueRequest(ev.RequestID),
				)

				if err != nil {
					fmt.Fprintf(os.Stderr, "continue request err: %s\n", err)
				}
			}()
		}
	}
}

func isThirdParty(base, sub string) bool {
	var err error
	base, err = publicsuffix.EffectiveTLDPlusOne(base)
	if err != nil {
		return false
	}

	sub, err = publicsuffix.EffectiveTLDPlusOne(sub)
	if err != nil {
		return false
	}

	return base != sub
}

type listArg []string

func (l *listArg) Set(val string) error {
	*l = append(*l, val)
	return nil
}

func (h listArg) String() string {
	return "string"
}
