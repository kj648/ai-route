package gateway

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"

	"ai-route/internal/store"
)

// orderGroup orders the members of a same-priority group by weighted
// rendezvous hashing: each member scores -ln(u)/weight with u derived from
// hash(affinity, member), lowest first. A member is first with probability
// proportional to its weight across conversations, the same conversation
// always gets the same order, and removing a member only moves the
// conversations that were on it. Without affinity u is random.
func orderGroup(members []store.TargetMember, affinity string) []string {
	if len(members) == 1 {
		return []string{members[0].Target}
	}
	type scored struct {
		target string
		score  float64
	}
	s := make([]scored, len(members))
	for i, m := range members {
		var u float64
		if affinity == "" {
			u = rand.Float64()
		} else {
			h := fnv.New64a()
			h.Write([]byte(affinity))
			h.Write([]byte{0})
			h.Write([]byte(m.Target))
			u = float64(h.Sum64()>>11) / (1 << 53)
		}
		u = math.Max(u, 1e-12)
		s[i] = scored{m.Target, -math.Log(u) / float64(m.Weight)}
	}
	sort.SliceStable(s, func(i, j int) bool { return s[i].score < s[j].score })
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.target
	}
	return out
}
