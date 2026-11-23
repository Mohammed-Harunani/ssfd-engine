package agents

import "fmt"

type spendState uint8

const (
	quiet spendState = iota
	routine
	active
)

// Rows are current states; columns are next states. Each sums to 100.
var transitionCutoffs = [3][2]uint64{{74, 97}, {17, 87}, {9, 44}}

func nextState(current spendState, draw uint64) spendState {
	cutoffs := transitionCutoffs[current]
	switch {
	case draw < cutoffs[0]:
		return quiet
	case draw < cutoffs[1]:
		return routine
	default:
		return active
	}
}

func (e *engine) daily() error {
	for i, actor := range e.p.retail {
		rng := &e.streams[i]
		// All bounds are positive by construction.
		move, _ := rng.Below(100)
		state := nextState(e.states[i], move)
		e.states[i] = state
		channel, _ := rng.Below(100)
		var err error
		switch {
		case channel < [3]uint64{26, 61, 78}[state]:
			pick, _ := rng.Below(2)
			n, _ := rng.Below([3]uint64{1200, 3500, 7000}[state])
			m := e.p.business[pick]
			_, err = e.transfer(Card, actor, m, actor.Accounts[0], m.Accounts[0], 300+int64(n))
		case channel >= 82 && channel < 91:
			other, _ := rng.Below(uint64(len(e.p.retail) - 1))
			j := int(other)
			if j >= i {
				j++
			}
			n, _ := rng.Below(5200)
			peer := e.p.retail[j]
			_, err = e.transfer(PeerToPeer, actor, peer, actor.Accounts[0], peer.Accounts[0], 200+int64(n))
		case channel >= 98:
			n, _ := rng.Below(9600)
			err = e.exchange(actor, e.p.business[2], 400+int64(n))
		}
		if err != nil {
			return fmt.Errorf("daily activity for actor %d: %w", actor.ID, err)
		}
	}
	return nil
}
