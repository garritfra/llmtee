// Command gen downloads LiteLLM's model price table and writes the compact
// subset llmtee embeds (internal/pricing/prices.json).
//
//	go run ./internal/pricing/gen
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
)

const defaultURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// Fields we keep. All prices are USD per token.
var keep = []string{
	"litellm_provider",
	"input_cost_per_token",
	"output_cost_per_token",
	"cache_read_input_token_cost",
	"cache_creation_input_token_cost",
	"cache_creation_input_token_cost_above_1hr",
	"input_cost_per_token_above_200k_tokens",
	"output_cost_per_token_above_200k_tokens",
	"cache_read_input_token_cost_above_200k_tokens",
	"cache_creation_input_token_cost_above_200k_tokens",
}

var modes = map[string]bool{"": true, "chat": true, "completion": true, "responses": true, "embedding": true}

func main() {
	src := flag.String("src", defaultURL, "URL or file path of model_prices_and_context_window.json")
	out := flag.String("out", "internal/pricing/prices.json", "output file")
	flag.Parse()

	var r io.Reader
	if strings.HasPrefix(*src, "http") {
		resp, err := http.Get(*src)
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			log.Fatalf("download: %s", resp.Status)
		}
		r = resp.Body
	} else {
		f, err := os.Open(*src)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		r = f
	}

	var all map[string]map[string]any
	if err := json.NewDecoder(r).Decode(&all); err != nil {
		log.Fatal(err)
	}

	names := make([]string, 0, len(all))
	for k := range all {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("{\n")
	n := 0
	for _, name := range names {
		e := all[name]
		if name == "sample_spec" {
			continue
		}
		mode, _ := e["mode"].(string)
		if !modes[mode] {
			continue
		}
		if _, ok := e["input_cost_per_token"].(float64); !ok {
			continue
		}
		m := map[string]any{}
		for _, k := range keep {
			if val, ok := e[k]; ok && val != nil {
				m[k] = val
			}
		}
		line, err := json.Marshal(m)
		if err != nil {
			log.Fatal(err)
		}
		key, _ := json.Marshal(name)
		if n > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "%s:%s", key, line)
		n++
	}
	b.WriteString("\n}\n")
	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d models to %s", n, *out)
}
