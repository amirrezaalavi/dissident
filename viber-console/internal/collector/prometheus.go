package collector

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PromSample is one metric family occurrence with its label set and value.
// For histogram families, the "+Inf" bucket is dropped and Buckets holds the
// cumulative bucket edges with their counts (Sum/Count are also populated).
type PromSample struct {
	Name    string
	Labels  map[string]string
	Value   float64
	Count   uint64
	Sum     float64
	Buckets []HistogramBucket // cumulative; only for histogram families
}

type HistogramBucket struct {
	Le    float64
	Count uint64
}

// ParsePrometheusText parses the subset of Prometheus text exposition format
// emitted by the viber stack (and standard for any Prom client): bare metric
// lines `name{labels} value` plus `_bucket`, `_sum`, `_count` histogram lines
// and `# HELP`/`# TYPE` comments. Returns samples keyed by metric family name
// (histogram family gets one PromSample with buckets attached).
func ParsePrometheusText(body string) (map[string][]PromSample, error) {
	families := make(map[string][]PromSample)
	histogramSeen := make(map[string]bool) // family name -> is histogram TYPE
	var raw []promLine

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "# TYPE ") {
				parts := strings.Fields(line)
				if len(parts) == 4 {
					histogramSeen[parts[2]] = parts[3] == "histogram"
				}
			}
			continue
		}
		sample, err := parsePromLine(line)
		if err != nil {
			return nil, err
		}
		raw = append(raw, sample)
	}

	// Group plain samples by family.
	plain := map[string][]PromSample{}
	for _, s := range raw {
		if s.isBucket || s.isSum || s.isCount {
			continue
		}
		plain[s.name] = append(plain[s.name], PromSample{
			Name:   s.name,
			Labels: s.labels,
			Value:  s.value,
		})
	}
	for name, samples := range plain {
		families[name] = samples
	}

	// Build histograms: family name from `name_bucket` lines.
	histByFamily := map[string][]HistogramBucket{}
	histCount := map[string]uint64{}
	histSum := map[string]float64{}
	for _, s := range raw {
		if s.isBucket {
			le, _ := strconv.ParseFloat(s.labels["le"], 64)
			if s.labels["le"] == "+Inf" {
				continue
			}
			histByFamily[s.family] = append(histByFamily[s.family], HistogramBucket{Le: le, Count: uint64(s.value)})
		}
		if s.isCount {
			histCount[s.family] = uint64(s.value)
		}
		if s.isSum {
			histSum[s.family] = s.value
		}
	}
	for family, buckets := range histByFamily {
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].Le < buckets[j].Le })
		families[family] = []PromSample{{
			Name:    family,
			Labels:  map[string]string{},
			Count:   histCount[family],
			Sum:     histSum[family],
			Buckets: buckets,
		}}
	}

	_ = histogramSeen // TYPE comments parsed for future strictness; not required

	return families, nil
}

type promLine struct {
	name    string
	family  string
	labels  map[string]string
	value   float64
	isBucket bool
	isSum    bool
	isCount  bool
}

func parsePromLine(line string) (promLine, error) {
	// Split off value (last whitespace token).
	sp := strings.LastIndex(line, " ")
	if sp < 0 {
		return promLine{}, fmt.Errorf("prometheus: malformed line %q", line)
	}
	namePart := strings.TrimSpace(line[:sp])
	valPart := strings.TrimSpace(line[sp+1:])

	value, err := strconv.ParseFloat(valPart, 64)
	if err != nil {
		return promLine{}, fmt.Errorf("prometheus: bad value in %q: %w", line, err)
	}

	// Strip labels: name{labels}
	name := namePart
	var labels map[string]string
	if lb := strings.IndexByte(namePart, '{'); lb >= 0 {
		if !strings.HasSuffix(namePart, "}") {
			return promLine{}, fmt.Errorf("prometheus: unbalanced braces in %q", line)
		}
		name = strings.TrimSpace(namePart[:lb])
		labels, err = parseLabels(namePart[lb+1 : len(namePart)-1])
		if err != nil {
			return promLine{}, err
		}
	}

	pl := promLine{name: name, labels: labels, value: value}
	switch {
	case strings.HasSuffix(name, "_bucket"):
		pl.isBucket = true
		pl.family = strings.TrimSuffix(name, "_bucket")
	case strings.HasSuffix(name, "_sum"):
		pl.isSum = true
		pl.family = strings.TrimSuffix(name, "_sum")
	case strings.HasSuffix(name, "_count"):
		pl.isCount = true
		pl.family = strings.TrimSuffix(name, "_count")
	}
	return pl, nil
}

func parseLabels(s string) (map[string]string, error) {
	labels := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return labels, nil
	}
	for _, pair := range splitLabelPairs(s) {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("prometheus: bad label pair %q", pair)
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		labels[key] = val
	}
	return labels, nil
}

// splitLabelPairs splits `a="1",b="2"` respecting quoted commas.
func splitLabelPairs(s string) []string {
	var pairs []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			pairs = append(pairs, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		pairs = append(pairs, strings.TrimSpace(cur.String()))
	}
	return pairs
}

// Percentile estimates the p-th percentile from a cumulative histogram using
// linear interpolation within the bucket. p is 0..100. Returns 0 when there
// are no buckets or the sample count is zero.
func Percentile(buckets []HistogramBucket, p float64) float64 {
	if len(buckets) == 0 {
		return 0
	}
	total := buckets[len(buckets)-1].Count
	if total == 0 {
		return 0
	}
	target := float64(total) * p / 100.0
	prevLe, prevCount := 0.0, uint64(0)
	for _, b := range buckets {
		if float64(b.Count) >= target {
			if b.Count == prevCount {
				return prevLe
			}
			frac := (target - float64(prevCount)) / float64(b.Count-prevCount)
			return prevLe + (b.Le-prevLe)*frac
		}
		prevLe, prevCount = b.Le, b.Count
	}
	return prevLe
}

// FindGauge returns the value of a gauge/counter sample with matching labels.
// Returns (0, false) if absent. Extra labels in the query must be a subset of
// the sample's labels.
func FindGauge(samples []PromSample, labels map[string]string) (float64, bool) {
	for _, s := range samples {
		if len(labels) > len(s.Labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s.Value, true
		}
	}
	return 0, false
}
