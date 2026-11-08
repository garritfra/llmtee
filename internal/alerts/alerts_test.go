package alerts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/garritfra/llmtee/internal/config"
	"github.com/garritfra/llmtee/internal/store"
)

func TestThresholdsFireOnce(t *testing.T) {
	var mu sync.Mutex
	var hooks []Alert
	var ntfy []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/hook" {
			var a Alert
			json.Unmarshal(b, &a)
			hooks = append(hooks, a)
		} else {
			ntfy = append(ntfy, r.Header.Get("Title")+": "+string(b))
		}
	}))
	defer srv.Close()
	n := New(config.Alerts{WebhookURL: srv.URL + "/hook", NtfyURL: srv.URL + "/topic", Thresholds: []float64{0.8, 1}, GlobalDayUSD: 100}, nil)
	day := 10.0
	k := &store.Key{ID: "mk-1", Name: "agent", BudgetDayUSD: &day}
	n.Check(k, store.KeySpend{Day: 5}, store.KeySpend{Day: 5})
	n.Check(k, store.KeySpend{Day: 8.5}, store.KeySpend{Day: 8.5}) // 80%
	n.Check(k, store.KeySpend{Day: 9}, store.KeySpend{Day: 9})     // still 80%: no repeat
	n.Check(k, store.KeySpend{Day: 10}, store.KeySpend{Day: 10})   // 100%
	n.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(hooks) != 2 || hooks[0].Threshold != 0.8 || hooks[1].Threshold != 1 || hooks[1].KeyName != "agent" {
		t.Fatalf("hooks = %+v", hooks)
	}
	if len(ntfy) != 2 {
		t.Fatalf("ntfy = %v", ntfy)
	}

	// Prime suppresses already-crossed thresholds after a restart.
	n2 := New(config.Alerts{WebhookURL: srv.URL + "/hook", Thresholds: []float64{0.8}}, nil)
	n2.Prime([]*store.Key{k}, func(string) store.KeySpend { return store.KeySpend{Day: 9} }, store.KeySpend{})
	n2.Check(k, store.KeySpend{Day: 9.5}, store.KeySpend{})
	n2.Wait()
	if len(hooks) != 2 {
		t.Fatalf("primed alert re-sent: %d", len(hooks))
	}
	if New(config.Alerts{}, nil) != nil {
		t.Error("notifier without channels should be nil")
	}
}
