package protocol

import "testing"

func FuzzFragmentAssemblersNeverPanic(f *testing.F) {
	f.Add([]byte{
		1, 7, 0, 2, 'A',
		1, 7, 3, 4, 'X',
		1, 7, 1, 2, 'B',
		1, 7, 0, 2, 'A',
	})
	f.Add([]byte{
		1, 9, 0, 4, 'A',
		1, 9, 1, 2, 'X',
		2, 9, 1, 4, 'Y',
	})
	f.Add([]byte{
		1, 3, 0, 0, 'A',
		1, 3, 0, 1, 'B',
		1, 3, 1, 2, 'C',
		1, 3, 1, 2, 'D',
		1, 3, 0, 2, 'E',
	})

	f.Fuzz(func(t *testing.T, sequence []byte) {
		if len(sequence) > 4096 {
			sequence = sequence[:4096]
		}

		regular := &FragmentAssembler{fragments: make(map[uint16]*fragmentGroup)}
		sharded := &ShardedFragmentAssembler{
			shards:     make([]fragmentShard, 4),
			shardCount: 4,
		}
		for i := range sharded.shards {
			sharded.shards[i].fragments = make(map[uint16]*fragmentGroup)
		}

		assemblers := []UDPFragmentAssembler{regular, sharded}
		for offset := 0; offset+5 <= len(sequence); offset += 5 {
			sessionID := uint32(sequence[offset])
			fragID := uint16(sequence[offset+1])
			index := sequence[offset+2]
			total := sequence[offset+3]
			payload := sequence[offset+4 : offset+5]

			for _, assembler := range assemblers {
				result, err := assembler.AddFragment(sessionID, fragID, index, total, payload)
				if result != nil && err != nil {
					t.Fatalf("assembler returned payload and error together: payload=%q, err=%v", result, err)
				}
			}
		}

		for _, group := range regular.fragments {
			releaseFragmentGroup(group)
		}
		for i := range sharded.shards {
			for _, group := range sharded.shards[i].fragments {
				releaseFragmentGroup(group)
			}
		}
	})
}
