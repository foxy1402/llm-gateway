package proxy

import (
	"crypto/rand"
	"math/big"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
)

// rotationPlan encapsulates combo member selection for one request.
type rotationPlan struct {
	comboID  string
	rotation config.RotationPolicy
	members  []string
	endpoint string
}

func (p *Proxy) newRotationPlan(combo *config.Combo, endpoint string) *rotationPlan {
	// Prefer members that support this endpoint.
	members := make([]string, 0, len(combo.Members))
	for _, pid := range combo.Members {
		prov := p.registry.GetProvider(pid)
		if prov == nil || !prov.Enabled {
			continue
		}
		members = append(members, pid)
	}
	return &rotationPlan{
		comboID:  combo.ID,
		rotation: combo.Rotation,
		members:  members,
		endpoint: endpoint,
	}
}

// next selects the next provider ID to try, returning "" when exhausted.
func (rp *rotationPlan) next(reg *registry.Registry, tried map[string]bool) string {
	eligible := func(pid string) bool {
		if tried[pid] {
			return false
		}
		if !reg.Health().IsAvailable(pid) {
			return false
		}
		if !reg.Health().SupportsEndpoint(pid, rp.endpoint) {
			return false
		}
		return true
	}

	available := func() []string {
		out := []string{}
		for _, m := range rp.members {
			if eligible(m) {
				out = append(out, m)
			}
		}
		return out
	}

	switch rp.rotation {
	case config.Random:
		avail := available()
		if len(avail) == 0 {
			return ""
		}
		// Weight-aware random (#10): providers with higher weight are picked more
		// often, proportionally. Falls back to uniform when no member has weight>0.
		totalWeight := 0
		weights := make([]int64, len(avail))
		for i, pid := range avail {
			w := int64(1)
			if prov := reg.GetProvider(pid); prov != nil && prov.Weight > 0 {
				w = int64(prov.Weight)
			}
			weights[i] = w
			totalWeight += int(w)
		}
		// Degenerate case: all weight<=0 or missing → uniform random.
		if totalWeight <= 0 {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(avail))))
			if err != nil {
				return avail[0]
			}
			return avail[n.Int64()]
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(totalWeight)))
		if err != nil {
			return avail[0]
		}
		r := n.Int64()
		for i, w := range weights {
			if r < w {
				return avail[i]
			}
			r -= w
		}
		return avail[len(avail)-1]

	case config.WeightedRoundRobin:
		// Smooth WRR via registry state.
		return reg.SelectWRR(rp.comboID, eligible)

	case config.Priority:
		// Members are already in position order.
		for _, m := range rp.members {
			if eligible(m) {
				return m
			}
		}
		return ""

	case config.RoundRobin:
		fallthrough
	default:
		avail := available()
		if len(avail) == 0 {
			return ""
		}
		n := reg.IncrementRR(rp.comboID)
		return avail[int(n%int64(len(avail)))]
	}
}
