// pulsebench is PulseHTTP's load-testing harness: raw-TCP keep-alive
// workers, exact latency percentiles from every recorded sample (no
// histogram approximation), status-code accounting, and JSON output for
// scripted regression comparisons.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	latencies []time.Duration
	statuses  map[int]int
	errors    int
}

func main() {
	var (
		target    = flag.String("url", "127.0.0.1:8080/", "host:port/path to hit")
		conc      = flag.Int("c", 50, "concurrent connections")
		total     = flag.Int("n", 100000, "total requests")
		keepAlive = flag.Bool("keepalive", true, "reuse connections")
		warmup    = flag.Int("warmup", 1000, "warmup requests (not measured)")
		hdrs      = flag.String("H", "", "extra headers, semicolon-separated (k: v;k2: v2)")
		asJSON    = flag.Bool("json", false, "emit machine-readable JSON")
	)
	flag.Parse()

	hostport, path, _ := strings.Cut(*target, "/")
	path = "/" + path
	request := buildRequest(hostport, path, *hdrs)

	// Warmup primes listen queues, caches, and the runtime scheduler.
	if *warmup > 0 {
		runLoad(hostport, request, min(*conc, 8), *warmup, *keepAlive)
	}

	start := time.Now()
	results := runLoad(hostport, request, *conc, *total, *keepAlive)
	elapsed := time.Since(start)

	report(results, elapsed, *conc, *keepAlive, *asJSON)
}

func buildRequest(hostport, path, extra string) []byte {
	var b strings.Builder
	b.WriteString("GET " + path + " HTTP/1.1\r\n")
	b.WriteString("Host: " + hostport + "\r\n")
	b.WriteString("User-Agent: pulsebench/1.0\r\n")
	for _, h := range strings.Split(extra, ";") {
		if h = strings.TrimSpace(h); h != "" {
			b.WriteString(h + "\r\n")
		}
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// runLoad fires exactly total requests across conc workers.
func runLoad(hostport string, request []byte, conc, total int, keepAlive bool) []result {
	var remaining atomic.Int64
	remaining.Store(int64(total))
	results := make([]result, conc)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			res := result{statuses: map[int]int{}}
			var conn net.Conn
			var br *bufio.Reader
			dial := func() bool {
				var err error
				conn, err = net.DialTimeout("tcp", hostport, 2*time.Second)
				if err != nil {
					res.errors++
					return false
				}
				br = bufio.NewReaderSize(conn, 32<<10)
				return true
			}
			defer func() {
				if conn != nil {
					conn.Close()
				}
			}()
			for remaining.Add(-1) >= 0 {
				if conn == nil && !dial() {
					continue
				}
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				t0 := time.Now()
				if _, err := conn.Write(request); err != nil {
					conn.Close()
					conn = nil
					res.errors++
					continue
				}
				status, err := readResponse(br)
				if err != nil {
					conn.Close()
					conn = nil
					res.errors++
					continue
				}
				res.latencies = append(res.latencies, time.Since(t0))
				res.statuses[status]++
				if !keepAlive {
					conn.Close()
					conn = nil
				}
			}
			results[slot] = res
		}(i)
	}
	wg.Wait()
	return results
}

// readResponse consumes one full response, returning its status code.
func readResponse(br *bufio.Reader) (int, error) {
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, err
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, fmt.Errorf("bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("bad status %q", parts[1])
	}
	contentLength := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, val, ok := strings.Cut(line, ":"); ok &&
			strings.EqualFold(name, "Content-Length") {
			contentLength, _ = strconv.Atoi(strings.TrimSpace(val))
		}
	}
	if contentLength > 0 {
		if _, err := io.CopyN(io.Discard, br, int64(contentLength)); err != nil {
			return 0, err
		}
	}
	return status, nil
}

func report(results []result, elapsed time.Duration, conc int, keepAlive, asJSON bool) {
	var all []time.Duration
	statuses := map[int]int{}
	errors := 0
	for _, r := range results {
		all = append(all, r.latencies...)
		errors += r.errors
		for s, n := range r.statuses {
			statuses[s] += n
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		idx := int(p*float64(len(all))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(all) {
			idx = len(all) - 1
		}
		return all[idx]
	}
	var sum time.Duration
	for _, d := range all {
		sum += d
	}
	completed := len(all)
	rps := float64(completed) / elapsed.Seconds()

	if asJSON {
		out := map[string]any{
			"completed": completed, "errors": errors, "elapsed_sec": elapsed.Seconds(),
			"rps": rps, "connections": conc, "keepalive": keepAlive,
			"statuses": statuses,
			"latency_ms": map[string]float64{
				"avg": msf(avg(sum, completed)), "p50": msf(pct(0.50)), "p90": msf(pct(0.90)),
				"p99": msf(pct(0.99)), "p999": msf(pct(0.999)), "max": msf(pct(1.0)),
			},
		}
		json.NewEncoder(os.Stdout).Encode(out)
		return
	}

	fmt.Printf("Requests     %d completed, %d errors in %.2fs\n", completed, errors, elapsed.Seconds())
	fmt.Printf("Throughput   %.0f req/s  (%d conns, keepalive=%v)\n", rps, conc, keepAlive)
	fmt.Printf("Latency      avg %s | p50 %s | p90 %s | p99 %s | p99.9 %s | max %s\n",
		ms(avg(sum, completed)), ms(pct(0.50)), ms(pct(0.90)), ms(pct(0.99)), ms(pct(0.999)), ms(pct(1.0)))
	keys := make([]int, 0, len(statuses))
	for s := range statuses {
		keys = append(keys, s)
	}
	sort.Ints(keys)
	fmt.Printf("Statuses     ")
	for _, s := range keys {
		fmt.Printf("%d:%d  ", s, statuses[s])
	}
	fmt.Println()
}

func avg(sum time.Duration, n int) time.Duration {
	if n == 0 {
		return 0
	}
	return sum / time.Duration(n)
}
func ms(d time.Duration) string  { return fmt.Sprintf("%.2fms", msf(d)) }
func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
