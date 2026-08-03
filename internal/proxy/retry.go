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
	members  []config.ComboMember
	endpoint string
}

func (p *Proxy) newRotationPlan(combo *config.Combo, endpoint string) *rotationPlan {
	// Keep enabled members that support this endpoint (provider-level check).
	members := make([]config.ComboMember, 0, len(combo.Members))
	for _, m := range combo.Members {
		prov := p.registry.GetProvider(m.ProviderID)
		if prov == nil || !prov.Enabled {
			continue
		}
		members = append(members, m)
	}
	return &rotationPlan{
		comboID:  combo.ID,
		rotation: combo.Rotation,
		members:  members,
		endpoint: endpoint,
	}
}

// next selects the next combo member (provider+model) to try, returning nil when exhausted.
func (rp *rotationPlan) next(reg *registry.Registry, tried map[string]bool) *config.ComboMember {
	eligible := func(m config.ComboMember) bool {
		if tried[m.ProviderID] {
			return false
		}
		if !reg.Health().IsAvailable(m.ProviderID) {
			return false
		}
		if !reg.Health().SupportsEndpoint(m.ProviderID, rp.endpoint) {
			return false
		}
		return true
	}

	available := func() []config.ComboMember {
		out := []config.ComboMember{}
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
			return nil
		}
		// Weight-aware random (#10): providers with higher weight are picked more
		// often, proportionally. Falls back to uniform when no member has weight>0.
		totalWeight := 0
		weights := make([]int64, len(avail))
		for i, m := range avail {
			w := int64(1)
			if prov := reg.GetProvider(m.ProviderID); prov != nil && prov.Weight > 0 {
				w = int64(prov.Weight)
			}
			weights[i] = w
			totalWeight += int(w)
		}
		// Degenerate case: all weight<=0 or missing → uniform random.
		if totalWeight <= 0 {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(avail))))
			if err != nil {
				return &avail[0]
			}
			return &avail[n.Int64()]
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(totalWeight)))
		if err != nil {
			return &avail[0]
		}
		r := n.Int64()
		for i, w := range weights {
			if r < w {
				return &avail[i]
			}
			r -= w
		}
		return &avail[len(avail)-1]

	case config.WeightedRoundRobin:
		// Smooth WRR via registry state.
		pid := reg.SelectWRR(rp.comboID, func(pid string) bool {
			for _, m := range rp.members {
				if m.ProviderID == pid {
					return eligible(m)
				}
			}
			return false
		})
		if pid == "" {
			return nil
		}
		for i := range rp.members {
			if rp.members[i].ProviderID == pid {
				return &rp.members[i]
			}
		}
		return nil

	case config.Priority:
		// Members are already in position order.
		for i := range rp.members {
			if eligible(rp.members[i]) {
				return &rp.members[i]
			}
		}
		return nil

	case config.RoundRobin:
		fallthrough
	default:
		avail := available()
		if len(avail) == 0 {
			return nil
		}
		n := reg.IncrementRR(rp.comboID)
		return &avail[int(n%int64(len(avail)))]
	}
}
