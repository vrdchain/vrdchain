// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2018-2019 The Soteria DAG developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockdag

import (
	"compress/bzip2"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/soteria-dag/soterd/blockdag/phantom"
	"github.com/soteria-dag/soterd/chaincfg"
	"github.com/soteria-dag/soterd/chaincfg/chainhash"
	"github.com/soteria-dag/soterd/database"
	_ "github.com/soteria-dag/soterd/database/ffldb"
	"github.com/soteria-dag/soterd/soterutil"
	"github.com/soteria-dag/soterd/txscript"
	"github.com/soteria-dag/soterd/wire"
)

const (
	// testDbType is the database backend type to use for the tests.
	testDbType = "ffldb"

	// testDbRoot is the root directory used to create all test databases.
	testDbRoot = "testdbs"

	// blockDataNet is the expected network in the test block data.
	blockDataNet = wire.MainNet
)

// filesExists returns whether or not the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// isSupportedDbType returns whether or not the passed database type is
// currently supported.
func isSupportedDbType(dbType string) bool {
	supportedDrivers := database.SupportedDrivers()
	for _, driver := range supportedDrivers {
		if dbType == driver {
			return true
		}
	}

	return false
}

// loadBlocks reads files containing soter block data (gzipped but otherwise
// in the format bitcoind writes) from disk and returns them as an array of
// soterutil.Block.  This is largely borrowed from the test code in database package.
func loadBlocks(filename string) (blocks []*soterutil.Block, err error) {
	filename = filepath.Join("testdata/", filename)

	var network = wire.MainNet
	var dr io.Reader
	var fi io.ReadCloser

	fi, err = os.Open(filename)
	if err != nil {
		return
	}

	if strings.HasSuffix(filename, ".bz2") {
		dr = bzip2.NewReader(fi)
	} else {
		dr = fi
	}
	defer fi.Close()

	var block *soterutil.Block

	err = nil
	for height := int64(1); err == nil; height++ {
		var rintbuf uint32
		err = binary.Read(dr, binary.LittleEndian, &rintbuf)
		if err == io.EOF {
			// hit end of file at expected offset: no warning
			height--
			err = nil
			break
		}
		if err != nil {
			break
		}
		if rintbuf != uint32(network) {
			break
		}
		err = binary.Read(dr, binary.LittleEndian, &rintbuf)
		blocklen := rintbuf

		rbytes := make([]byte, blocklen)

		// read block
		dr.Read(rbytes)

		block, err = soterutil.NewBlockFromBytes(rbytes)
		if err != nil {
			return
		}
		blocks = append(blocks, block)
	}

	return
}

// chainSetup is used to create a new db and chain instance with the genesis
// block already inserted.  In addition to the new chain instance, it returns
// a teardown function the caller should invoke when done testing to clean up.
func chainSetup(dbName string, params *chaincfg.Params) (*BlockDAG, func(), error) {
	if !isSupportedDbType(testDbType) {
		return nil, nil, fmt.Errorf("unsupported db type %v", testDbType)
	}

	// Handle memory database specially since it doesn't need the disk
	// specific handling.
	var db database.DB
	var teardown func()
	if testDbType == "memdb" {
		ndb, err := database.Create(testDbType)
		if err != nil {
			return nil, nil, fmt.Errorf("error creating db: %v", err)
		}
		db = ndb

		// Setup a teardown function for cleaning up.  This function is
		// returned to the caller to be invoked when it is done testing.
		teardown = func() {
			db.Close()
		}
	} else {
		// Create the root directory for test databases.
		if !fileExists(testDbRoot) {
			if err := os.MkdirAll(testDbRoot, 0700); err != nil {
				err := fmt.Errorf("unable to create test db "+
					"root: %v", err)
				return nil, nil, err
			}
		}

		// Create a new database to store the accepted blocks into.
		dbPath := filepath.Join(testDbRoot, dbName)
		_ = os.RemoveAll(dbPath)
		ndb, err := database.Create(testDbType, dbPath, blockDataNet)
		if err != nil {
			return nil, nil, fmt.Errorf("error creating db: %v", err)
		}
		db = ndb

		// Setup a teardown function for cleaning up.  This function is
		// returned to the caller to be invoked when it is done testing.
		teardown = func() {
			db.Close()
			os.RemoveAll(dbPath)
			os.RemoveAll(testDbRoot)
		}
	}

	// Copy the chain params to ensure any modifications the tests do to
	// the chain parameters do not affect the global instance.
	paramsCopy := *params

	// Create the main chain instance.
	chain, err := New(&Config{
		DB:          db,
		ChainParams: &paramsCopy,
		// NOTE(cedric): Commented out to disable checkpoint-related code (JIRA DAG-3)
		// 
		//Checkpoints: nil,
		TimeSource: NewMedianTime(),
		SigCache:   txscript.NewSigCache(1000),
	})
	if err != nil {
		teardown()
		err := fmt.Errorf("failed to create chain instance: %v", err)
		return nil, nil, err
	}
	return chain, teardown, nil
}

// loadUtxoView returns a utxo view loaded from a file.
func loadUtxoView(filename string) (*UtxoViewpoint, error) {
	// The utxostore file format is:
	// <tx hash><output index><serialized utxo len><serialized utxo>
	//
	// The output index and serialized utxo len are little endian uint32s
	// and the serialized utxo uses the format described in chainio.go.

	filename = filepath.Join("testdata", filename)
	fi, err := os.Open(filename)
	if err != nil {
		return nil, err
	}

	// Choose read based on whether the file is compressed or not.
	var r io.Reader
	if strings.HasSuffix(filename, ".bz2") {
		r = bzip2.NewReader(fi)
	} else {
		r = fi
	}
	defer fi.Close()

	view := NewUtxoViewpoint()
	for {
		// Hash of the utxo entry.
		var hash chainhash.Hash
		_, err := io.ReadAtLeast(r, hash[:], len(hash[:]))
		if err != nil {
			// Expected EOF at the right offset.
			if err == io.EOF {
				break
			}
			return nil, err
		}

		// Output index of the utxo entry.
		var index uint32
		err = binary.Read(r, binary.LittleEndian, &index)
		if err != nil {
			return nil, err
		}

		// Num of serialized utxo entry bytes.
		var numBytes uint32
		err = binary.Read(r, binary.LittleEndian, &numBytes)
		if err != nil {
			return nil, err
		}

		// Serialized utxo entry.
		serialized := make([]byte, numBytes)
		_, err = io.ReadAtLeast(r, serialized, int(numBytes))
		if err != nil {
			return nil, err
		}

		// Deserialize it and add it to the view.
		entry, err := deserializeUtxoEntry(serialized)
		if err != nil {
			return nil, err
		}
		view.Entries()[wire.OutPoint{Hash: hash, Index: index}] = entry
	}

	return view, nil
}


// TstSetCoinbaseMaturity makes the ability to set the coinbase maturity
// available when running tests.
func (b *BlockDAG) TstSetCoinbaseMaturity(maturity uint16) {
	b.chainParams.CoinbaseMaturity = maturity
}

// newFakeChain returns a chain that is usable for syntetic tests.  It is
// important to note that this chain has no database associated with it, so
// it is not usable with all functions and the tests must take care when making
// use of it.
func newFakeChain(params *chaincfg.Params) *BlockDAG {
	// Create a genesis block node and block index index populated with it
	// for use when creating the fake chain below.
	node := newBlockNode(&params.GenesisBlock.Header, nil, nil)
	index := newBlockIndex(nil, params)
	index.AddNode(node)

	targetTimespan := int64(params.TargetTimespan / time.Millisecond)
	targetTimePerBlock := int64(params.TargetTimePerBlock / time.Millisecond)
	adjustmentFactor := params.RetargetAdjustmentFactor
	dag := BlockDAG{
		chainParams:         params,
		timeSource:          NewMedianTime(),
		minRetargetTimespan: targetTimespan / adjustmentFactor,
		maxRetargetTimespan: targetTimespan * adjustmentFactor,
		blocksPerRetarget:   int64(targetTimespan / targetTimePerBlock),
		index:               index,
		dView:               newDAGView([]*blockNode{node}),
		graph:               phantom.NewGraph(),
		blueSet:             phantom.NewBlueSetCache(),
		nodeOrder:           make([]*chainhash.Hash, 0),
		warningCaches:       newThresholdCaches(vbNumBits),
		deploymentCaches:    newThresholdCaches(chaincfg.DefinedDeployments),
	}

	dag.graph.AddNodeById(params.GenesisBlock.Header.BlockHash().String())
	dag.dagSnapshot = newDAGState([]*blockNode{node}, 1)

	return &dag
}

// newFakeNode creates a block node connected to the passed parent with the
// provided fields populated and fake values for the other fields.
func newFakeNode(parent *blockNode, blockVersion int32, bits uint32, timestamp time.Time) *blockNode {
	// Make up a header and create a block node from it.
	prevHash := generateTipsHash([]*blockNode{parent})
	header := &wire.BlockHeader{
		Version:   blockVersion,
		PrevBlock: *prevHash,
		Bits:      bits,
		Timestamp: timestamp,
	}

	parentHeader := &wire.ParentSubHeader{
		Version: 1,
		Parents: []*wire.Parent{{ Hash: parent.hash }},
		Size:    1,
	}
	return newBlockNode(header, parentHeader, []*blockNode{parent})
}
