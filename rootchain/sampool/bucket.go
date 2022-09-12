package sampool

import (
	"github.com/0xPolygon/polygon-edge/rootchain"
	"github.com/0xPolygon/polygon-edge/types"
)

type samSet struct {
	messages   []rootchain.SAM
	signatures map[string]bool
}

func newSet() samSet {
	return samSet{
		messages:   make([]rootchain.SAM, 0),
		signatures: make(map[string]bool),
	}
}

func (s *samSet) add(msg rootchain.SAM) {
	strSignature := string(msg.Signature)

	if s.signatures[strSignature] {
		return
	}

	s.messages = append(s.messages, msg)
	s.signatures[strSignature] = true
}

func (s *samSet) get() []rootchain.SAM {
	return s.messages
}

type samBucket map[types.Hash]samSet

func newBucket() samBucket {
	return make(map[types.Hash]samSet)
}

func (b samBucket) add(msg rootchain.SAM) {
	messages, ok := b[msg.Hash]
	if !ok {
		messages = newSet()
	}

	messages.add(msg)
	b[msg.Hash] = messages
}

type quorumFunc func(uint64) bool

func (b samBucket) getQuorumMessages(quorum quorumFunc) []rootchain.SAM {
	for _, set := range b {
		messages := set.get()

		if quorum(uint64(len(messages))) {
			return messages
		}
	}

	return nil
}
