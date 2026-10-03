// Command burst simulates an on-sale stampede: N users hitting the same seat
// at the same instant, followed by a reconciliation check against the show.
//
//	go run ./cmd/burst -show <id> -seat A12 -n 500
//	./burst.sh --url https://seatlock.fly.dev <show-id> A12 500
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type result struct {
	status  int
	reason  string
	latency time.Duration
}

func main() {
	var (
		base    = flag.String("url", "http://localhost:8080", "base URL of the API")
		showID  = flag.String("show", "", "show id (required)")
		seat    = flag.String("seat", "A12", "seat label every request fights over")
		n       = flag.Int("n", 500, "number of simultaneous requests")
		oneUser = flag.Bool("one-user", false, "send every request as the same user (tests the per-user limit instead of the seat race)")
		spread  = flag.Bool("spread", false, "give each request a DIFFERENT seat (A1, A2, ...) instead of all fighting over -seat")
	)
	flag.Parse()

	if *showID == "" {
		fmt.Fprintln(os.Stderr, "error: -show is required")
		os.Exit(2)
	}

	// A load generator must not be the bottleneck. Go's default of 2 idle
	// connections per host would serialise most of this burst.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *n + 10,
			MaxIdleConnsPerHost: *n + 10,
			MaxConnsPerHost:     *n + 10,
		},
	}

	// Distinguishes one run from the next, so re-running against the same show
	// sends fresh requests instead of 500 idempotent replays.
	runID := time.Now().UnixNano()

	// Tokens are minted BEFORE the barrier. Minting inside the goroutines
	// would stagger the start and the burst would not be simultaneous.
	fmt.Printf("minting %d tokens...\n", *n)
	tokens := make([]string, *n)
	var mintWG sync.WaitGroup
	sem := make(chan struct{}, 32)
	for i := range tokens {
		mintWG.Add(1)
		go func(i int) {
			defer mintWG.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			user := fmt.Sprintf("user_%d", i)
			if *oneUser {
				user = "solo_user"
			}
			tokens[i] = mintToken(client, *base, user)
		}(i)
	}
	mintWG.Wait()

	results := make([]result, *n)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// -spread aims every request at its own seat, so the seat race
			// cannot mask whatever else is being tested (e.g. the per-user
			// quota, which only bites when seats are NOT contended).
			target := *seat
			if *spread {
				target = fmt.Sprintf("A%d", i+1)
			}

			body, _ := json.Marshal(map[string]any{
				"seats":           []string{target},
				"idempotency_key": fmt.Sprintf("burst-%d-%d", runID, i),
			})

			// Every goroutine parks here. Closing the channel releases all of
			// them in the same scheduling instant - this is the "stampede".
			<-start

			t0 := time.Now()
			req, err := http.NewRequest(http.MethodPost,
				*base+"/shows/"+*showID+"/reserve", bytes.NewReader(body))
			if err != nil {
				results[i] = result{status: 0, reason: "request_build_failed"}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tokens[i])

			resp, err := client.Do(req)
			lat := time.Since(t0)
			if err != nil {
				results[i] = result{status: 0, reason: "transport: " + condense(err), latency: lat}
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			var parsed struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &parsed)
			reason := parsed.Error
			if resp.StatusCode < 300 {
				reason = "confirmed"
			}
			results[i] = result{status: resp.StatusCode, reason: reason, latency: lat}
		}(i)
	}

	fmt.Printf("firing %d simultaneous requests at seat %s...\n\n", *n, *seat)
	wallStart := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(wallStart)

	won := 0
	for _, r := range results {
		if r.status == http.StatusCreated {
			won++
		}
	}

	report(results, wall)
	ok := reconcile(client, *base, *showID, won)

	if !ok {
		os.Exit(1)
	}
}

func mintToken(c *http.Client, base, user string) string {
	resp, err := c.Post(base+"/dev/token?user_id="+user, "application/json", nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Token
}

func report(results []result, wall time.Duration) {
	byOutcome := map[string]int{}
	var fiveXX int
	lat := make([]time.Duration, 0, len(results))

	for _, r := range results {
		key := fmt.Sprintf("%d %s", r.status, r.reason)
		byOutcome[key]++
		if r.status >= 500 || r.status == 0 {
			fiveXX++
		}
		if r.latency > 0 {
			lat = append(lat, r.latency)
		}
	}

	keys := make([]string, 0, len(byOutcome))
	for k := range byOutcome {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Println("---- outcomes ----")
	for _, k := range keys {
		fmt.Printf("  %-32s %5d\n", k, byOutcome[k])
	}
	fmt.Printf("  %-32s %5d\n", "5xx / transport failures", fiveXX)

	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		fmt.Println("\n---- latency ----")
		fmt.Printf("  p50 %-8v p95 %-8v p99 %-8v max %v\n",
			pct(lat, 50).Round(time.Millisecond),
			pct(lat, 95).Round(time.Millisecond),
			pct(lat, 99).Round(time.Millisecond),
			lat[len(lat)-1].Round(time.Millisecond))
		fmt.Printf("  wall %v  throughput %.0f req/s\n",
			wall.Round(time.Millisecond),
			float64(len(results))/wall.Seconds())
	}
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p)/100 - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// reconcile runs two independent checks, because neither catches everything.
//
//  1. available + held + confirmed == total
//     Catches leaked and duplicated seat rows.
//
//  2. 201 responses == newly confirmed seats
//     Catches the case the invariant is BLIND to: when two writers overwrite
//     the same seat row, one reservation silently loses its seat. The row
//     count never changes, so check 1 still passes while two users have each
//     been told they own A12. Only comparing promises to seats finds it.
func reconcile(c *http.Client, base, showID string, won int) bool {
	resp, err := c.Get(base + "/shows/" + showID)
	if err != nil {
		fmt.Printf("\nreconciliation failed: %v\n", err)
		return false
	}
	defer resp.Body.Close()

	var state struct {
		Summary struct {
			Available int `json:"available"`
			Held      int `json:"held"`
			Confirmed int `json:"confirmed"`
			Total     int `json:"total"`
		} `json:"summary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		fmt.Printf("\nreconciliation failed: %v\n", err)
		return false
	}

	s := state.Summary
	sum := s.Available + s.Held + s.Confirmed
	fmt.Println("\n---- reconciliation ----")

	seatMath := sum == s.Total
	fmt.Printf("  seat math     available %d + held %d + confirmed %d = %d / total %d   %s\n",
		s.Available, s.Held, s.Confirmed, sum, s.Total, verdict(seatMath))

	noDoubleSell := won == s.Confirmed
	fmt.Printf("  double-sell   %d requests told 201, %d seats actually confirmed        %s\n",
		won, s.Confirmed, verdict(noDoubleSell))

	if seatMath && noDoubleSell {
		fmt.Println("  " + strings.Repeat("=", 20))
		fmt.Println("  ALL CHECKS PASS")
		return true
	}

	fmt.Println("  " + strings.Repeat("!", 20))
	if !seatMath {
		fmt.Printf("  SEAT MATH BROKEN: %d accounted for, %d exist\n", sum, s.Total)
	}
	if !noDoubleSell {
		fmt.Printf("  DOUBLE-SELL: %d users hold a confirmation for only %d seats\n", won, s.Confirmed)
	}
	return false
}

func verdict(ok bool) string {
	if ok {
		return "OK"
	}
	return "FAIL"
}

// condense shortens a transport error to its distinguishing tail, so the
// outcome table groups identical failures instead of printing 500 variants.
func condense(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		s = s[i+2:]
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
