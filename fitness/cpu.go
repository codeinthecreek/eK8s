package fitness

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// maxMetricsBytes caps the kubelet metrics text ParseResource reads.
const maxMetricsBytes = 16 << 20

// podCPU is the kubelet resource metric ParseResource reads: a pod's
// cumulative CPU time in core-seconds (kubelet /metrics/resource).
const podCPU = "pod_cpu_usage_seconds_total"

// Pod names a pod.
type Pod struct{ Namespace, Name string }

// Sample is one read of the kubelet's cumulative pod CPU counters.
type Sample map[Pod]float64

// ParseResource reads the Prometheus text the kubelet serves at
// /metrics/resource (through the API server: kubectl get --raw
// /api/v1/nodes/<node>/proxy/metrics/resource) and returns its
// pod_cpu_usage_seconds_total series. Other series, comments and blank
// lines are skipped. A series of that name that is malformed, has no
// namespace or pod label, repeats a pod, or carries a value that is not
// finite and non-negative is an error: a counter read wrong must not
// become a CPU figure.
func ParseResource(r io.Reader) (Sample, error) { return parseResource(r, maxMetricsBytes) }

// parseResource is ParseResource with the input cap as a parameter. An
// input over max bytes is an error, not a silent cut: a truncated read
// could drop a pod's series and undercount its CPU.
func parseResource(r io.Reader, max int64) (Sample, error) {
	out := make(Sample)
	lr := &io.LimitedReader{R: r, N: max + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if !strings.HasPrefix(line, podCPU+"{") {
			continue
		}
		labels, rest, err := parseLabels(line[len(podCPU)+1:])
		if err != nil {
			return nil, fmt.Errorf("fitness: metrics line %d: %w", n, err)
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 || len(fields) > 2 {
			return nil, fmt.Errorf("fitness: metrics line %d: want a value and at most a timestamp", n)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("fitness: metrics line %d: value %q", n, fields[0])
		}
		ns, pod := labels["namespace"], labels["pod"]
		if ns == "" || pod == "" {
			return nil, fmt.Errorf("fitness: metrics line %d: no namespace or pod label", n)
		}
		k := Pod{ns, pod}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("fitness: metrics line %d: pod %s/%s twice", n, ns, pod)
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("fitness: reading metrics: %w", err)
	}
	if lr.N <= 0 {
		return nil, fmt.Errorf("fitness: metrics over %d bytes", max)
	}
	return out, nil
}

// parseLabels parses `k="v",k="v"} rest` (the text after the opening
// brace), with the backslash escapes of the Prometheus text format.
func parseLabels(s string) (labels map[string]string, rest string, err error) {
	labels = make(map[string]string)
	for {
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], nil
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 || len(s) < eq+2 || s[eq+1] != '"' {
			return nil, "", errors.New("malformed label")
		}
		key := s[:eq]
		var val strings.Builder
		i := eq + 2
		for {
			if i >= len(s) {
				return nil, "", errors.New("unterminated label value")
			}
			c := s[i]
			if c == '"' {
				break
			}
			if c == '\\' {
				i++
				if i >= len(s) {
					return nil, "", errors.New("unterminated label value")
				}
				switch s[i] {
				case '\\', '"':
					c = s[i]
				case 'n':
					c = '\n'
				default:
					return nil, "", errors.New("unknown escape in a label value")
				}
			}
			val.WriteByte(c)
			i++
		}
		if _, dup := labels[key]; dup {
			return nil, "", errors.New("label repeated")
		}
		labels[key] = val.String()
		s = s[i+1:]
		s = strings.TrimPrefix(s, ",")
	}
}

// CPUSeconds is the CPU time the pods of namespace spent between two
// samples: the sum, over its pods in end, of end minus start, a pod
// absent from start (created in the window, its counter starting at 0)
// counting from 0. It is an error, not a figure, when
//   - namespace has no pod in end;
//   - a pod of namespace in start is gone in end, since its final
//     counter is lost and the half's CPU would be undercounted (an HPA
//     scale-down, a deletion: the half is excluded);
//   - a counter went down (a reset).
func CPUSeconds(start, end Sample, namespace string) (float64, error) {
	var sum float64
	n := 0
	for pod, e := range end {
		if pod.Namespace != namespace {
			continue
		}
		n++
		s := start[pod]
		if e < s {
			return 0, fmt.Errorf("fitness: pod %s/%s's counter went from %v to %v", pod.Namespace, pod.Name, s, e)
		}
		sum += e - s
	}
	if n == 0 {
		return 0, fmt.Errorf("fitness: no pod in namespace %s at the end sample", namespace)
	}
	var gone []string
	for pod := range start {
		if _, ok := end[pod]; !ok && pod.Namespace == namespace {
			gone = append(gone, pod.Name)
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		return 0, fmt.Errorf("fitness: pods %s of namespace %s are gone at the end sample", strings.Join(gone, ", "), namespace)
	}
	return sum, nil
}
