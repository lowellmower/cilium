// Copyright 2016-2017 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package policy

import (
	"encoding/json"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/policy/api/v3"
)

// Repository is a list of policy rules which in combination form the security
// policy. A policy repository can be
type Repository struct {
	// Mutex protects the whole policy tree
	Mutex lock.RWMutex
	rules []*rule

	// revision is the revision of the policy repository. It will be
	// incremented whenever the policy repository is changed
	revision uint64
}

// NewPolicyRepository allocates a new policy repository
func NewPolicyRepository() *Repository {
	return &Repository{}
}

// traceState is an internal structure used to collect information
// while determining policy decision
type traceState struct {
	// selectedRules is the number of rules with matching EndpointSelector
	selectedRules int

	// matchedRules is the number of rules that have allowed traffic
	matchedRules int

	// constrainedRules counts how many "FromRequires" constraints are
	// unsatisfied
	constrainedRules int

	// ruleID is the rule ID currently being evaluated
	ruleID int
}

func (state *traceState) trace(p *Repository, ctx *SearchContext) {
	ctx.PolicyTrace("%d/%d rules selected\n", state.selectedRules, len(p.rules))
	if state.constrainedRules > 0 {
		ctx.PolicyTrace("Found unsatisfied FromRequires constraint\n")
	} else if state.matchedRules > 0 {
		ctx.PolicyTrace("Found allow rule\n")
	} else {
		ctx.PolicyTrace("Found no allow rule\n")
	}
}

// CanReachIngressRLocked evaluates the policy repository for the provided search
// context and returns the verdict or v3.Undecided if no rule matches for
// ingress. The policy repository mutex must be held.
func (p *Repository) CanReachIngressRLocked(ctx *SearchContext) v3.Decision {
	decision := v3.Undecided
	state := traceState{}

loop:
	for i, r := range p.rules {
		state.ruleID = i
		switch r.canReachIngress(ctx, &state) {
		// The rule contained a constraint which was not met, this
		// connection is not allowed
		case v3.Denied:
			decision = v3.Denied
			break loop

		// The rule allowed the connection but a later rule may impose
		// additional constraints, so we store the decision but allow
		// it to be overwritten by an additional requirement
		case v3.Allowed:
			decision = v3.Allowed
		}
	}

	state.trace(p, ctx)

	return decision
}

// AllowsIngressLabelAccess evaluates the policy repository for the provided search
// context and returns the verdict for ingress policy. If no matching policy
// allows for the  connection, the request will be denied. The policy repository
// mutex must be held.
func (p *Repository) AllowsIngressLabelAccess(ctx *SearchContext) v3.Decision {
	ctx.PolicyTrace("Tracing %s\n", ctx.String())
	decision := v3.Denied

	if len(p.rules) == 0 {
		ctx.PolicyTrace("  No rules found\n")
	} else {
		if p.CanReachIngressRLocked(ctx) == v3.Allowed {
			decision = v3.Allowed
		}
	}

	ctx.PolicyTrace("Label verdict: %s", decision.String())

	return decision
}

// ResolveL4Policy resolves the L4 policy for a set of endpoints by searching
// the policy repository for `PortRule` rules that are attached to a `Rule`
// where the EndpointSelector matches `ctx.To`. `ctx.From` takes no effect and
// is ignored in the search.  If multiple `PortRule` rules are found, all rules
// are merged together. If rules contains overlapping port definitions, the first
// rule found in the repository takes precedence.
//
// TODO: Coalesce l7 rules?
func (p *Repository) ResolveL4Policy(ctx *SearchContext) (*L4Policy, error) {
	result := NewL4Policy()

	ctx.PolicyTrace("\n")
	if ctx.EgressL4Only {
		ctx.PolicyTrace("Resolving egress port policy for %+v\n", ctx.To)
	} else if ctx.IngressL4Only {
		ctx.PolicyTrace("Resolving ingress port policy for %+v\n", ctx.To)
	} else {
		ctx.PolicyTrace("Resolving port policy for %+v\n", ctx.To)
	}

	state := traceState{}
	for _, r := range p.rules {
		found, err := r.resolveL4Policy(ctx, &state, result)
		if err != nil {
			return nil, err
		}
		state.ruleID++
		if found != nil {
			state.matchedRules++
		}
	}

	if result != nil {
		result.Revision = p.GetRevision()
	}

	state.trace(p, ctx)
	return result, nil
}

// ResolveCIDRPolicy resolves the L3 policy for a set of endpoints by searching
// the policy repository for `CIDR` rules that are attached to a `Rule`
// where the EndpointSelector matches `ctx.To`. `ctx.From` takes no effect and
// is ignored in the search.
func (p *Repository) ResolveCIDRPolicy(ctx *SearchContext) *CIDRPolicy {
	result := NewCIDRPolicy()

	ctx.PolicyTrace("Resolving L3 (CIDR) policy for %+v\n", ctx.To)

	state := traceState{}
	for _, r := range p.rules {
		r.resolveCIDRPolicy(ctx, &state, result)
		state.ruleID++
	}

	state.trace(p, ctx)
	return result
}

func (p *Repository) allowsL4Egress(searchCtx *SearchContext) v3.Decision {
	ctx := *searchCtx
	ctx.To = ctx.From
	ctx.From = labels.LabelArray{}
	ctx.EgressL4Only = true

	policy, err := p.ResolveL4Policy(&ctx)
	if err != nil {
		log.WithError(err).Warn("Evaluation error while resolving L4 egress policy")
	}
	verdict := v3.Undecided
	if err == nil && len(policy.Egress) > 0 {
		verdict = policy.EgressCoversDPorts(ctx.DPorts)
	}

	if len(ctx.DPorts) == 0 {
		ctx.PolicyTrace("L4 egress verdict: [no port context specified]")
	} else {
		ctx.PolicyTrace("L4 egress verdict: %s", verdict.String())
	}

	return verdict
}

func (p *Repository) allowsL4Ingress(ctx *SearchContext) v3.Decision {
	ctx.IngressL4Only = true

	policy, err := p.ResolveL4Policy(ctx)
	if err != nil {
		log.WithError(err).Warn("Evaluation error while resolving L4 ingress policy")
	}
	verdict := v3.Undecided
	if err == nil && len(policy.Ingress) > 0 {
		verdict = policy.IngressCoversContext(ctx)
	}

	if len(ctx.DPorts) == 0 {
		ctx.PolicyTrace("L4 ingress verdict: [no port context specified]")
	} else {
		ctx.PolicyTrace("L4 ingress verdict: %s", verdict.String())
	}

	return verdict
}

// AllowsIngressRLocked evaluates the policy repository for the provided search
// context and returns the verdict for ingress. If no matching policy allows for
// the  connection, the request will be denied. The policy repository mutex must
// be held.
func (p *Repository) AllowsIngressRLocked(ctx *SearchContext) v3.Decision {
	ctx.PolicyTrace("Tracing %s\n", ctx.String())
	decision := p.CanReachIngressRLocked(ctx)
	ctx.PolicyTrace("Label verdict: %s", decision.String())
	if decision == v3.Allowed {
		ctx.PolicyTrace("L4 ingress & egress policies skipped")
		return decision
	}

	// We only report the overall decision as L4 inclusive if a port has
	// been specified
	if len(ctx.DPorts) != 0 {
		l4Egress := p.allowsL4Egress(ctx)
		l4Ingress := p.allowsL4Ingress(ctx)

		// Explicit deny should deny; Allow+Undecided should allow
		if l4Egress == v3.Denied || l4Ingress == v3.Denied {
			decision = v3.Denied
		} else if l4Egress == v3.Allowed || l4Ingress == v3.Allowed {
			decision = v3.Allowed
		}
	}

	if decision != v3.Allowed {
		decision = v3.Denied
	}
	return decision
}

// AllowsEgressRLocked evaluates the policy repository for the provided search
// context and returns the verdict. If no matching policy allows for the
// connection, the request will be denied. The policy repository mutex must be
// held.
func (p *Repository) AllowsEgressRLocked(egressCtx *SearchContext) v3.Decision {
	egressCtx.PolicyTrace("Tracing %s\n", egressCtx.String())
	egressDecision := p.CanReachEgressRLocked(egressCtx)
	egressCtx.PolicyTrace("Egress label verdict: %s", egressDecision.String())

	if egressDecision == v3.Allowed {
		egressCtx.PolicyTrace("L4 egress policies skipped")
		return egressDecision
	}
	if len(egressCtx.DPorts) != 0 {
		egressDecision = p.allowsL4Egress(egressCtx)
	}

	if egressDecision != v3.Allowed {
		egressDecision = v3.Denied
	}

	return egressDecision
}

// AllowsEgressLabelAccess evaluates the policy repository for the provided search
// context and returns the verdict for egress. If no matching
// policy allows for the connection, the request will be denied.
// The policy repository mutex must be held.
func (p *Repository) AllowsEgressLabelAccess(egressCtx *SearchContext) v3.Decision {
	egressCtx.PolicyTrace("Tracing %s\n", egressCtx.String())
	egressDecision := v3.Denied
	if len(p.rules) == 0 {
		egressCtx.PolicyTrace("  No rules found\n")
	} else {
		egressDecision = p.CanReachEgressRLocked(egressCtx)
	}

	egressCtx.PolicyTrace("Egress label verdict: %s", egressDecision.String())

	return egressDecision
}

// CanReachEgressRLocked evaluates the policy repository for the provided search
// context and returns the verdict or api.Undecided if no rule matches for egress
// policy.
// The policy repository mutex must be held.
func (p *Repository) CanReachEgressRLocked(egressCtx *SearchContext) v3.Decision {
	egressDecision := v3.Undecided
	egressState := traceState{}

egressLoop:
	for i, r := range p.rules {
		egressState.ruleID = i
		switch r.canReachEgress(egressCtx, &egressState) {
		// The rule contained a constraint which was not met, this
		// connection is not allowed
		case v3.Denied:
			egressDecision = v3.Denied
			break egressLoop

			// The rule allowed the connection but a later rule may impose
			// additional constraints, so we store the decision but allow
			// it to be overwritten by an additional requirement
		case v3.Allowed:
			egressDecision = v3.Allowed
		}
	}

	egressState.trace(p, egressCtx)

	return egressDecision
}

// SearchRLocked searches the policy repository for rules which match the
// specified labels and will return an array of all rules which matched.
func (p *Repository) SearchRLocked(labels labels.LabelArray) v3.Rules {
	result := v3.Rules{}

	for _, r := range p.rules {
		if r.Labels.Contains(labels) {
			result = append(result, &r.Rule)
		}
	}

	return result
}

// Add inserts a rule into the policy repository
// This is just a helper function for unit testing.
func (p *Repository) Add(r v3.Rule) (uint64, error) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if err := r.Sanitize(); err != nil {
		return p.revision, err
	}

	newList := make([]*v3.Rule, 1)
	newList[0] = &r
	return p.AddListLocked(newList)
}

// AddListLocked inserts a rule into the policy repository with the repository already locked
// Expects that the entire rule list has already been sanitized.
func (p *Repository) AddListLocked(rules v3.Rules) (uint64, error) {
	newList := make([]*rule, len(rules))
	for i := range rules {
		newList[i] = &rule{Rule: *rules[i]}
	}
	p.rules = append(p.rules, newList...)
	p.revision++
	metrics.PolicyCount.Add(float64(len(newList)))
	metrics.PolicyRevision.Inc()

	return p.revision, nil
}

// AddList inserts a rule into the policy repository
func (p *Repository) AddList(rules v3.Rules) (uint64, error) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()
	return p.AddListLocked(rules)
}

// DeleteByLabelsLocked deletes all rules in the policy repository which
// contain the specified labels
func (p *Repository) DeleteByLabelsLocked(labels labels.LabelArray) (uint64, int) {
	deleted := 0
	new := p.rules[:0]

	for _, r := range p.rules {
		if !r.Labels.Contains(labels) {
			new = append(new, r)
		} else {
			deleted++
		}
	}

	if deleted > 0 {
		p.revision++
		p.rules = new
		metrics.PolicyCount.Sub(float64(deleted))
		metrics.PolicyRevision.Inc()
	}

	return p.revision, deleted
}

// DeleteByLabels deletes all rules in the policy repository which contain the
// specified labels
func (p *Repository) DeleteByLabels(labels labels.LabelArray) (uint64, int) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()
	return p.DeleteByLabelsLocked(labels)
}

// JSONMarshalRules returns a slice of policy rules as string in JSON
// representation
func JSONMarshalRules(rules v3.Rules) string {
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// GetJSON returns all rules of the policy repository as string in JSON
// representation
func (p *Repository) GetJSON() string {
	p.Mutex.RLock()
	defer p.Mutex.RUnlock()

	result := v3.Rules{}
	for _, r := range p.rules {
		result = append(result, &r.Rule)
	}

	return JSONMarshalRules(result)
}

// GetRulesMatching returns whether any of the rules in a repository contain a
// rule with labels matching the labels in the provided LabelArray.
// If includeEntities is true, we check if repository contains rules matching
// fromEntities and toEntities.
//
// Must be called with p.Mutex held
func (p *Repository) GetRulesMatching(labels labels.LabelArray) (ingressMatch bool, egressMatch bool) {
	ingressMatch = false
	egressMatch = false
	for _, r := range p.rules {
		rulesMatch := r.EndpointSelector.Matches(labels)
		if rulesMatch {
			if len(r.Ingress) > 0 {
				ingressMatch = true
			}
			if len(r.Egress) > 0 {
				egressMatch = true
			}
		}

		if ingressMatch && egressMatch {
			return
		}
	}
	return
}

// NumRules returns the amount of rules in the policy repository.
//
// Must be called with p.Mutex held
func (p *Repository) NumRules() int {
	return len(p.rules)
}

// GetRevision returns the revision of the policy repository
func (p *Repository) GetRevision() uint64 {
	return p.revision
}

// TranslateRules traverses rules and applies provided translator to rules
func (p *Repository) TranslateRules(translator Translator) error {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	for ruleIndex := range p.rules {
		if err := translator.Translate(&p.rules[ruleIndex].Rule); err != nil {
			return err
		}
	}
	return nil
}

// BumpRevision allows forcing policy regeneration
func (p *Repository) BumpRevision() {
	metrics.PolicyRevision.Inc()
	p.Mutex.Lock()
	defer p.Mutex.Unlock()
	p.revision++
}

// GetRulesList returns the current policy
func (p *Repository) GetRulesList() *models.Policy {
	p.Mutex.RLock()
	defer p.Mutex.RUnlock()

	lbls := labels.ParseSelectLabelArrayFromArray([]string{})
	ruleList := p.SearchRLocked(lbls)

	return &models.Policy{
		Revision: int64(p.GetRevision()),
		Policy:   JSONMarshalRules(ruleList),
	}
}
