package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pulsecheck/internal/config"
)

// BenchmarkCheck measures one full check against a local 200 OK server: the
// request build, the round trip over the client's keep-alive connection, and
// the outcome classification. The server is started once and the timer is reset
// after it is up, so the reported numbers cover only Check itself.
//
// The client is srv.Client(), i.e. exactly the transport the tests and the
// scheduler use for a plain HTTP target; the code path under measurement is
// identical to production, which is what keeps the benchmark meaningful
// alongside -race runs of the same package.
func BenchmarkCheck(b *testing.B) {
	srv := httptest.NewServer(okHandler())
	defer srv.Close()

	client := srv.Client()
	defer client.CloseIdleConnections()

	target := config.Target{
		Name:     "bench",
		URL:      srv.URL,
		Method:   http.MethodGet,
		Interval: time.Second,
		Timeout:  5 * time.Second,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := Check(context.Background(), target, client)
		if res.Outcome != OutcomeUp {
			b.Fatalf("Check outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
		}
	}
}
