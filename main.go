package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

var fieldOptions = []string{
	"urlkey",
	"timestamp",
	"original",
	"mimetype",
	"statuscode",
	"digest",
	"length",
}

var matchOptions = []string{
	"exact",
	"prefix",
	"host",
	"domain",
}

var docs = `
waymachine - query the Wayback CDX Server API

Usage:
  waymachine -t <target> [options]

Description:
  Query archived captures from the Wayback CDX Server API.
  Targets may be a domain, host, or URL-like string.

  Requests that fail with HTTP 429 or 5xx are retried up to 3 times
  with exponential backoff.

Output:
  Results are written as JSON Lines (JSONL) to stdout by default.
  Use -o to write to a file instead.

Options:
  -t <target>     Target to query (required).

  -c <collapse>   Collapse results by field or field prefix (repeatable).
                  Format: field or field:N
                  Default: urlkey

  -f <fields>     Comma-separated list of fields to include.
                  Default: original

                  Available fields:
                    urlkey, timestamp, original, mimetype,
                    statuscode, digest, length

  -k <keys>       Rename output fields. Must match -f field count.
                  Format: key1,key2,...

  -m <match>      Match type for results.
                  Default: domain

                  Values:
                    exact     exact match of target
                    prefix    match target prefix (implies /*)
                    host      match all paths for host
                    domain    match domain and subdomains

  -r <filter>     Apply regex filter on a field (repeatable).
                  Format: [!]field:regex
                  Prefix with ! to negate.

                  Uses Java regex syntax.

                  Examples:
                    -r statuscode:^30[12]$
                    -r !statuscode:^2\d\d$
                    -r !mimetype:image

  -x <seconds>    Request timeout in seconds.
                  Default: 60

  -l <limit>      Maximum number of results.
                  Default: 1000

  -o <file>       Write output to file instead of stdout.
                  Parent directories are created if needed.

  -h, --help      Show this help and exit.
`

func usage() {
	fmt.Fprintf(os.Stderr, "%s\n", docs)
	os.Exit(0)
}

func log(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "waymachine: "+format+"\n", args...)
}

func hint() {
	log("try 'waymachine -h' for more information")
}

func die(code int, format string, args ...any) {
	log(format, args...)

	if code == 2 {
		hint()
	}

	os.Exit(code)
}

func retryable(err error, res *http.Response) bool {
	if err != nil {
		return true
	}

	if res != nil && (res.StatusCode == 429 || res.StatusCode >= 500) {
		return true
	}

	return false
}

func fetchWithRetry(client *http.Client, endpoint string) (*http.Response, error) {
	var res *http.Response
	var err error

	retries := 3
	backoff := 10 * time.Second

	for attempt := range retries {
		if attempt > 0 {
			log("Retry attempt %d of %d", attempt, retries)
		}

		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("User-Agent", "waymachine/1.0")

		res, err := client.Do(req)
		if !retryable(err, res) {
			if err != nil && res != nil {
				res.Body.Close()
			}
			return res, err
		}

		if res != nil {
			res.Body.Close()
		}

		time.Sleep(backoff)
		backoff *= 2
	}

	return res, err
}

func strToSlice(s string) []string {
	s = strings.Join(strings.Fields(s), "")
	if s == "" {
		return nil
	}

	return strings.Split(s, ",")
}

func main() {
	if len(os.Args) == 1 {
		hint()
		os.Exit(2)
	}

	var (
		help     bool
		target   string
		match    string
		fields   string
		keys     string
		output   string
		limit    uint
		timeout  uint
		filter   []string
		collapse []string
	)

	fs := flag.NewFlagSet("waymachine", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.BoolVar(&help, "h", false, "")
	fs.BoolVar(&help, "help", false, "")
	fs.StringVar(&target, "t", "", "")
	fs.StringVar(&match, "m", "domain", "")
	fs.StringVar(&fields, "f", "original", "")
	fs.StringVar(&keys, "k", "", "")
	fs.StringVar(&output, "o", "", "")

	fs.UintVar(&limit, "l", 1000, "")
	fs.UintVar(&timeout, "x", 60, "")

	fs.Func("r", "", func(s string) error {
		parts := strings.SplitN(s, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("%q filter must be followed by a regex", s)
		}

		field := strings.TrimPrefix(parts[0], "!")
		if !slices.Contains(fieldOptions, field) {
			return fmt.Errorf("%q is not a valid filter field", field)
		}

		filter = append(filter, s)

		return nil
	})

	fs.Func("c", "", func(s string) error {
		parts := strings.SplitN(s, ":", 2)

		if !slices.Contains(fieldOptions, parts[0]) {
			return fmt.Errorf("%q is not a valid collapse field", parts[0])
		}

		if len(parts) == 2 && parts[1] != "" {
			if _, err := strconv.Atoi(parts[1]); err != nil {
				return fmt.Errorf("collapse substring must be numeric")
			}
		}

		collapse = append(collapse, s)

		return nil
	})

	err := fs.Parse(os.Args[1:])
	if err != nil {
		die(2, "%v", err)
	}

	if help {
		usage()
	}

	if target == "" {
		die(2, "no target provided")
	}

	if !slices.Contains(matchOptions, match) {
		die(2, "%q is not a valid match option", match)
	}

	fieldList := strToSlice(fields)
	keyList := strToSlice(keys)

	for _, v := range fieldList {
		if !slices.Contains(fieldOptions, v) {
			die(2, "%q is not a valid field option", v)
		}
	}

	if len(keyList) == 0 {
		keyList = fieldList
	}

	if len(keyList) != len(fieldList) {
		die(2, "number of keys and fields do not match")
	}

	var writer io.Writer = os.Stdout
	if output != "" {
		err := os.MkdirAll(filepath.Dir(output), 0755)
		if err != nil {
			die(1, "failed to create output directory: %v", err)
		}

		f, err := os.Create(output)
		if err != nil {
			die(1, "failed to create output file: %v", err)
		}

		defer f.Close()

		writer = f
	}

	params := url.Values{}
	params.Set("url", strings.TrimRight(target, "/")+"/")
	params.Set("matchType", match)
	params.Set("limit", strconv.FormatUint(uint64(limit), 10))
	params.Set("fl", fields)
	params.Set("output", "json")

	for _, v := range filter {
		params.Add("filter", v)
	}

	if len(collapse) > 0 {
		for _, v := range collapse {
			params.Add("collapse", v)
		}
	} else {
		params.Set("collapse", "urlkey")
	}

	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}

	endpoint := "https://web.archive.org/cdx/search/cdx?" + params.Encode()

	res, err := fetchWithRetry(client, endpoint)
	if err != nil {
		die(1, "request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		die(1, "request failed after retries: %s", res.Status)
	}

	dec := json.NewDecoder(res.Body)

	t, err := dec.Token()
	if err != nil || t != json.Delim('[') {
		die(1, "invalid JSON response")
	}

	if !dec.More() {
		log("no results")
		return
	}

	var discard []string
	if err := dec.Decode(&discard); err != nil {
		die(1, "failed to decode header: %v", err)
	}

	enc := json.NewEncoder(writer)

	for dec.More() {
		var row []string
		if err := dec.Decode(&row); err != nil {
			die(1, "failed to decode row: %v", err)
		}

		obj := make(map[string]string, len(keyList))

		for i, v := range row {
			if i < len(keyList) {
				obj[keyList[i]] = v
			}
		}

		if err := enc.Encode(obj); err != nil {
			die(1, "failed to write output: %v", err)
		}
	}

	_, _ = dec.Token()
}
