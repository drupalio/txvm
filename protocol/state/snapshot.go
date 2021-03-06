/*
Package state defines Snapshot, a data structure for holding a
blockchain's state.
*/
package state

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/chain/txvm/errors"
	"github.com/chain/txvm/protocol/bc"
	"github.com/chain/txvm/protocol/patricia"
)

// Snapshot contains a blockchain's state.
//
// TODO: consider making type Snapshot truly immutable.  We already
// handle it that way in many places (with explicit calls to Copy to
// get the right behavior).  PruneNonces and the Apply functions would
// have to produce new Snapshots rather than updating Snapshots in
// place.
type Snapshot struct {
	ContractsTree *patricia.Tree
	NonceTree     *patricia.Tree

	Header         *bc.BlockHeader
	InitialBlockID bc.Hash
	RefIDs         []bc.Hash
}

// PruneNonces modifies a Snapshot, removing all nonce IDs with
// expiration times earlier than the provided timestamp.
func (s *Snapshot) PruneNonces(timestampMS uint64) {
	newTree := new(patricia.Tree)
	*newTree = *s.NonceTree

	patricia.Walk(s.NonceTree, func(item []byte) error {
		_, t := idTime(item)
		if timestampMS > t {
			newTree.Delete(item)
		}
		return nil
	})

	s.NonceTree = newTree
}

// Copy makes a copy of provided snapshot. Copying a snapshot is an
// O(n) operation where n is the number of nonces in the snapshot's
// nonce set.
func Copy(original *Snapshot) *Snapshot {
	c := &Snapshot{
		ContractsTree:  new(patricia.Tree),
		NonceTree:      new(patricia.Tree),
		InitialBlockID: original.InitialBlockID,
		RefIDs:         append([]bc.Hash{}, original.RefIDs...),
	}
	*c.ContractsTree = *original.ContractsTree
	*c.NonceTree = *original.NonceTree
	if original.Header != nil {
		c.Header = new(bc.BlockHeader)
		*c.Header = *original.Header
	}
	return c
}

// Empty returns an empty state snapshot.
func Empty() *Snapshot {
	return &Snapshot{
		ContractsTree: new(patricia.Tree),
		NonceTree:     new(patricia.Tree),
	}
}

// ApplyBlock updates s in place. It runs in three phases:
// PruneNonces, ApplyBlockHeader, and ApplyTx
// (the latter called in a loop for each transaction). Callers
// are free to invoke those phases separately.
func (s *Snapshot) ApplyBlock(block *bc.Block) error {
	s.PruneNonces(block.TimestampMs)

	err := s.ApplyBlockHeader(block.BlockHeader)
	if err != nil {
		return errors.Wrap(err, "applying block header")
	}

	for i, tx := range block.Transactions {
		err = s.ApplyTx(block.TimestampMs, tx)
		if err != nil {
			return errors.Wrapf(err, "applying block transaction %d", i)
		}
	}

	return nil
}

// ApplyBlockHeader is the header-specific phase of applying a block
// to the blockchain state. (See ApplyBlock.)
func (s *Snapshot) ApplyBlockHeader(bh *bc.BlockHeader) error {
	bHash := bh.Hash()

	if s.InitialBlockID.IsZero() {
		if bh.Height != 1 {
			return fmt.Errorf("cannot apply block with height %d to an empty state", bh.Height)
		}
		s.InitialBlockID = bHash
	} else if bh.Height == 1 {
		return fmt.Errorf("cannot apply block with height = 1 to an initialized state")
	}

	s.Header = bh
	s.RefIDs = append(s.RefIDs, bHash)

	return nil
}

// ApplyTx updates s in place.
func (s *Snapshot) ApplyTx(blockTimeMS uint64, tx *bc.Tx) error {
	if s.InitialBlockID.IsZero() {
		return fmt.Errorf("cannot apply a transaction to an empty state")
	}

	if blockTimeMS > math.MaxInt64 {
		return fmt.Errorf("block timestamp %d out of int64 range", blockTimeMS)
	}

	for _, tr := range tx.Timeranges {
		if tr.MaxMS > 0 && int64(blockTimeMS) > tr.MaxMS {
			return fmt.Errorf("block timestamp %d outside transaction time range %d-%d", blockTimeMS, tr.MinMS, tr.MaxMS)
		}
		if tr.MinMS > 0 && int64(blockTimeMS) > 0 && int64(blockTimeMS) < tr.MinMS {
			return fmt.Errorf("block timestamp %d outside transaction time range %d-%d", blockTimeMS, tr.MinMS, tr.MaxMS)
		}
	}

	nonceTree := new(patricia.Tree)
	*nonceTree = *s.NonceTree

	for _, n := range tx.Nonces {
		// Add new nonces. They must not conflict with nonces already
		// present.
		nc := NonceCommitment(n.ID, n.ExpMS)
		if nonceTree.Contains(nc) {
			return fmt.Errorf("conflicting nonce %x", n.ID.Bytes())
		}

		if n.BlockID.IsZero() || n.BlockID == s.InitialBlockID {
			// ok
		} else {
			var found bool
			for _, id := range s.RefIDs {
				if id == n.BlockID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("nonce must refer to the initial block, a recent block, or have a zero block ID")
			}
		}
		nonceTree.Insert(nc)
	}

	conTree := new(patricia.Tree)
	*conTree = *s.ContractsTree

	// Add or remove contracts, depending on if it is an input or output
	for _, con := range tx.Contracts {
		switch con.Type {
		case bc.InputType:
			if !conTree.Contains(con.ID.Bytes()) {
				return fmt.Errorf("invalid prevout %x", con.ID.Bytes())
			}
			conTree.Delete(con.ID.Bytes())

		case bc.OutputType:
			err := conTree.Insert(con.ID.Bytes())
			if err != nil {
				return err
			}
		}
	}

	s.NonceTree = nonceTree
	s.ContractsTree = conTree

	return nil
}

// Height returns the height from the stored latest header.
func (s *Snapshot) Height() uint64 {
	if s == nil || s.Header == nil {
		return 0
	}
	return s.Header.Height
}

// TimestampMS returns the timestamp from the stored latest header.
func (s *Snapshot) TimestampMS() uint64 {
	if s == nil || s.Header == nil {
		return 0
	}
	return s.Header.TimestampMs
}

// NonceCommitment returns the byte commitment
// for the given nonce id and expiration.
func NonceCommitment(id bc.Hash, expms uint64) []byte {
	b := make([]byte, 40)
	copy(b[:32], id.Bytes())
	binary.LittleEndian.PutUint64(b[32:], expms)
	return b
}

func idTime(b []byte) (bc.Hash, uint64) {
	h := bc.HashFromBytes(b[:32])
	t := binary.LittleEndian.Uint64(b[32:])
	return h, t
}
