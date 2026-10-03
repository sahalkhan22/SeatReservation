// Package metrics exposes the handful of counters that actually answer
// questions during an on-sale: how many bookings landed, why the rest were
// declined, how many seats are left, and whether the pool is saturating.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	Confirmed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "seatlock_reservations_confirmed_total",
		Help: "Reservations that resulted in seats being booked.",
	})

	// Labelled by reason, because "how many declines" is never the question -
	// the question is whether they were lost races (healthy) or limit hits.
	Declined = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "seatlock_reservations_declined_total",
		Help: "Reservations declined, by domain reason.",
	}, []string{"reason"})

	SeatsGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seatlock_seats",
		Help: "Seats per show by status.",
	}, []string{"show_id", "status"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "seatlock_request_duration_seconds",
		Help: "Request latency by route and status code.",
		// Buckets tuned for a lock-contended path: most of the interesting
		// signal is between 10ms and 1s.
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"route", "code"})

	PoolInUse = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "seatlock_db_pool_acquired",
		Help: "Connections currently checked out of the pool.",
	})
)

func Handler() http.Handler { return promhttp.Handler() }

// Observe records one request. route is the pattern, not the path, so 10,000
// show ids do not become 10,000 time series.
func Observe(route string, code int, d time.Duration) {
	RequestDuration.WithLabelValues(route, strconv.Itoa(code)).Observe(d.Seconds())
}
