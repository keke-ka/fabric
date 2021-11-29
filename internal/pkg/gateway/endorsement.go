/*
Copyright 2021 IBM All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package gateway

import (
	"bytes"
	"sync"

	"github.com/hyperledger/fabric-protos-go/peer"
)

type layout struct {
	required     map[string]int // group -> quantity
	endorsements []*peer.ProposalResponse
}

// The plan structure is initialised with an endorsement plan from discovery. It is used to manage the
// set of peers to be targeted for endorsement, to collect endorsements received from peers, to provide
// alternative peers for retry in the case of failure, and to trigger when the policy is satisfied.
// Note that this structure and its methods assume that each endorsing peer is in one and only one group.
// This is a constraint of the current algorithm in the discovery service.
type plan struct {
	layouts        []*layout
	groupEndorsers map[string][]*endorser // group -> endorsing peers
	groupIds       map[string]string      // peer pkiid -> group
	nextLayout     int
	size           int
	planLock       sync.Mutex
}

// construct and initialise an endorsement plan
func newPlan(layouts []*layout, groupEndorsers map[string][]*endorser) *plan {
	// Initialize the size with the total number of endorsers across all plans.  This is required to
	// determine the theoretical maximum channel buffer size when requesting endorsement across multiple goroutines.
	var size int
	groupIds := map[string]string{}
	for group, endorsers := range groupEndorsers {
		size += len(endorsers)
		for _, endorser := range endorsers {
			groupIds[endorser.pkiid.String()] = group
		}
	}
	return &plan{
		layouts:        layouts,
		groupEndorsers: groupEndorsers,
		groupIds:       groupIds,
		size:           size,
		planLock:       sync.Mutex{},
	}
}

// endorsers returns an array of endorsing peers representing the next layout in the list of available layouts
func (p *plan) endorsers() []*endorser {
	p.planLock.Lock()
	defer p.planLock.Unlock()

	var endorsers []*endorser
	for endorsers == nil {
		// Skip over any defunct layouts.
		for p.nextLayout < len(p.layouts) && p.layouts[p.nextLayout] == nil {
			p.nextLayout++
		}
		if p.nextLayout >= len(p.layouts) {
			return nil
		}
		for group, qty := range p.layouts[p.nextLayout].required {
			if qty > len(p.groupEndorsers[group]) {
				// requires more group endorsers than available - abandon this layout
				endorsers = nil
				p.layouts[p.nextLayout] = nil
				p.nextLayout++
				break
			}
			// remove the first qty endorsers from the front of this group
			endorsers = append(endorsers, p.groupEndorsers[group][0:qty]...)
			p.groupEndorsers[group] = p.groupEndorsers[group][qty:]
		}
	}
	return endorsers
}

// Invoke update when an endorsement has been successfully received for the given endorser.
// All layouts containing the group that contains this endorser are updated with the endorsement.
// Returns array of endorsements, if at least one layout in the plan has been satisfied, otherwise nil.
func (p *plan) update(endorser *endorser, endorsement *peer.ProposalResponse) []*peer.ProposalResponse {
	p.planLock.Lock()
	defer p.planLock.Unlock()

	group := p.groupIds[endorser.pkiid.String()]
	endorsers := p.groupEndorsers[group]

	// remove the endorser from this group
	// this is required if the given endorser was not originally provided by this plan instance (e.g. firstEndorser)
	for i, e := range endorsers {
		if bytes.Equal(e.pkiid, endorser.pkiid) {
			p.groupEndorsers[group] = append(endorsers[:i], endorsers[i+1:]...)
			break
		}
	}

	for i := p.nextLayout; i < len(p.layouts); i++ {
		layout := p.layouts[i]
		if layout == nil {
			continue
		}
		if quantity, ok := layout.required[group]; ok {
			layout.required[group] = quantity - 1
			layout.endorsements = append(layout.endorsements, endorsement)
			if layout.required[group] == 0 {
				// this group for this layout is complete - remove from map
				delete(layout.required, group)
				if len(layout.required) == 0 {
					// no groups left - this layout is now satisfied
					return layout.endorsements
				}
			}
		}
	}
	return nil
}

// Invoke nextPeerInGroup if an endorsement fails for the given endorser.
// Returns the next endorser in the same group as given endorser with which to retry the proposal, or nil if there are no more.
func (p *plan) nextPeerInGroup(endorser *endorser) *endorser {
	p.planLock.Lock()
	defer p.planLock.Unlock()

	group := p.groupIds[endorser.pkiid.String()]

	// next endorser for this layout
	if len(p.groupEndorsers[group]) > 0 {
		next := p.groupEndorsers[group][0]
		p.groupEndorsers[group] = p.groupEndorsers[group][1:]
		return next
	}

	// There are no more peers in this group, so will abandon this group entirely and remove all layouts that use it
	// Clearly the current layout was using it, so remove that
	// To avoid memory re-allocations, mark them as nil
	for i := p.nextLayout; i < len(p.layouts); i++ {
		layout := p.layouts[i]
		if layout != nil {
			if _, exists := layout.required[group]; exists {
				p.layouts[i] = nil
			}
		}
	}
	// continue with the next layout
	p.nextLayout++

	return nil
}