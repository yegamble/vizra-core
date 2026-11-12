package httpapi

import (
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// unmatchedRoute is the route label for a request no route matched. Using the
// raw path instead would put asset ids, user ids and probing garbage into label
// values: unbounded cardinality and a privacy leak.
const unmatchedRoute = "unmatched"

// httpMetricsMiddleware records request count and latency by method, matched
// route TEMPLATE and status code. Never the raw path or query string.
func httpMetricsMiddleware(reg prometheus.Registerer) echo.MiddlewareFunc {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vizra_http_requests_total",
		Help: "HTTP requests served, by method, route template and status code.",
	}, []string{"method", "route", "code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vizra_http_request_duration_seconds",
		Help:    "HTTP request latency, by method and route template.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
	reg.MustRegister(requests, duration)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			start := time.Now()
			err := next(c)
			route := c.RouteInfo().Path
			if route == "" {
				route = unmatchedRoute
			}
			code := 0
			if rw, uerr := echo.UnwrapResponse(c.Response()); uerr == nil {
				code = rw.Status
			}
			if err != nil {
				if he, ok := err.(echo.HTTPStatusCoder); ok {
					code = he.StatusCode()
				}
			}
			method := c.Request().Method
			requests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
			duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
			return err
		}
	}
}
