// Copyright 2016 The go-athereum Authors
// This file is part of the go-athereum library.
//
// The go-athereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-athereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-athereum library. If not, see <http://www.gnu.org/licenses/>.

// Package les implements the Light Atlantis Subprotocol.
package les

import (
	"fmt"
	"sync"
	"time"

	"github.com/athereum/go-athereum/accounts"
	"github.com/athereum/go-athereum/common"
	"github.com/athereum/go-athereum/common/hexutil"
	"github.com/athereum/go-athereum/consensus"
	"github.com/athereum/go-athereum/core"
	"github.com/athereum/go-athereum/core/bloombits"
	"github.com/athereum/go-athereum/core/rawdb"
	"github.com/athereum/go-athereum/core/types"
	"github.com/athereum/go-athereum/ath"
	"github.com/athereum/go-athereum/ath/downloader"
	"github.com/athereum/go-athereum/ath/filters"
	"github.com/athereum/go-athereum/ath/gasprice"
	"github.com/athereum/go-athereum/athdb"
	"github.com/athereum/go-athereum/event"
	"github.com/athereum/go-athereum/internal/athapi"
	"github.com/athereum/go-athereum/light"
	"github.com/athereum/go-athereum/log"
	"github.com/athereum/go-athereum/node"
	"github.com/athereum/go-athereum/p2p"
	"github.com/athereum/go-athereum/p2p/discv5"
	"github.com/athereum/go-athereum/params"
	rpc "github.com/athereum/go-athereum/rpc"
)

type LightAtlantis struct {
	config *ath.Config

	odr         *LesOdr
	relay       *LesTxRelay
	chainConfig *params.ChainConfig
	// Channel for shutting down the service
	shutdownChan chan bool
	// Handlers
	peers           *peerSet
	txPool          *light.TxPool
	blockchain      *light.LightChain
	protocolManager *ProtocolManager
	serverPool      *serverPool
	reqDist         *requestDistributor
	retriever       *retrieveManager
	// DB interfaces
	chainDb athdb.Database // Block chain database

	bloomRequests                              chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer, chtIndexer, bloomTrieIndexer *core.ChainIndexer

	ApiBackend *LesApiBackend

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	networkId     uint64
	netRPCService *athapi.PublicNetAPI

	wg sync.WaitGroup
}

func New(ctx *node.ServiceContext, config *ath.Config) (*LightAtlantis, error) {
	chainDb, err := ath.CreateDB(ctx, config, "lightchaindata")
	if err != nil {
		return nil, err
	}
	chainConfig, genesisHash, genesisErr := core.SetupGenesisBlock(chainDb, config.Genesis)
	if _, isCompat := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !isCompat {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	peers := newPeerSet()
	quitSync := make(chan struct{})

	lath := &LightAtlantis{
		config:           config,
		chainConfig:      chainConfig,
		chainDb:          chainDb,
		eventMux:         ctx.EventMux,
		peers:            peers,
		reqDist:          newRequestDistributor(peers, quitSync),
		accountManager:   ctx.AccountManager,
		engine:           ath.CreateConsensusEngine(ctx, &config.Ethash, chainConfig, chainDb),
		shutdownChan:     make(chan bool),
		networkId:        config.NetworkId,
		bloomRequests:    make(chan chan *bloombits.Retrieval),
		bloomIndexer:     ath.NewBloomIndexer(chainDb, light.BloomTrieFrequency),
		chtIndexer:       light.NewChtIndexer(chainDb, true),
		bloomTrieIndexer: light.NewBloomTrieIndexer(chainDb, true),
	}

	lath.relay = NewLesTxRelay(peers, lath.reqDist)
	lath.serverPool = newServerPool(chainDb, quitSync, &lath.wg)
	lath.retriever = newRetrieveManager(peers, lath.reqDist, lath.serverPool)
	lath.odr = NewLesOdr(chainDb, lath.chtIndexer, lath.bloomTrieIndexer, lath.bloomIndexer, lath.retriever)
	if lath.blockchain, err = light.NewLightChain(lath.odr, lath.chainConfig, lath.engine); err != nil {
		return nil, err
	}
	lath.bloomIndexer.Start(lath.blockchain)
	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		lath.blockchain.SetHead(compat.RewindTo)
		rawdb.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}

	lath.txPool = light.NewTxPool(lath.chainConfig, lath.blockchain, lath.relay)
	if lath.protocolManager, err = NewProtocolManager(lath.chainConfig, true, ClientProtocolVersions, config.NetworkId, lath.eventMux, lath.engine, lath.peers, lath.blockchain, nil, chainDb, lath.odr, lath.relay, lath.serverPool, quitSync, &lath.wg); err != nil {
		return nil, err
	}
	lath.ApiBackend = &LesApiBackend{lath, nil}
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.GasPrice
	}
	lath.ApiBackend.gpo = gasprice.NewOracle(lath.ApiBackend, gpoParams)
	return lath, nil
}

func lesTopic(genesisHash common.Hash, protocolVersion uint) discv5.Topic {
	var name string
	switch protocolVersion {
	case lpv1:
		name = "LES"
	case lpv2:
		name = "LES2"
	default:
		panic(nil)
	}
	return discv5.Topic(name + "@" + common.Bytes2Hex(genesisHash.Bytes()[0:8]))
}

type LightDummyAPI struct{}

// Atlantisbase is the address that mining rewards will be send to
func (s *LightDummyAPI) Atlantisbase() (common.Address, error) {
	return common.Address{}, fmt.Errorf("not supported")
}

// Coinbase is the address that mining rewards will be send to (alias for Atlantisbase)
func (s *LightDummyAPI) Coinbase() (common.Address, error) {
	return common.Address{}, fmt.Errorf("not supported")
}

// Hashrate returns the POW hashrate
func (s *LightDummyAPI) Hashrate() hexutil.Uint {
	return 0
}

// Mining returns an indication if this node is currently mining.
func (s *LightDummyAPI) Mining() bool {
	return false
}

// APIs returns the collection of RPC services theatlantis package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *LightAtlantis) APIs() []rpc.API {
	return append(athapi.GetAPIs(s.ApiBackend), []rpc.API{
		{
			Namespace: "ath",
			Version:   "1.0",
			Service:   &LightDummyAPI{},
			Public:    true,
		}, {
			Namespace: "ath",
			Version:   "1.0",
			Service:   downloader.NewPublicDownloaderAPI(s.protocolManager.downloader, s.eventMux),
			Public:    true,
		}, {
			Namespace: "ath",
			Version:   "1.0",
			Service:   filters.NewPublicFilterAPI(s.ApiBackend, true),
			Public:    true,
		}, {
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		},
	}...)
}

func (s *LightAtlantis) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *LightAtlantis) BlockChain() *light.LightChain      { return s.blockchain }
func (s *LightAtlantis) TxPool() *light.TxPool              { return s.txPool }
func (s *LightAtlantis) Engine() consensus.Engine           { return s.engine }
func (s *LightAtlantis) LesVersion() int                    { return int(s.protocolManager.SubProtocols[0].Version) }
func (s *LightAtlantis) Downloader() *downloader.Downloader { return s.protocolManager.downloader }
func (s *LightAtlantis) EventMux() *event.TypeMux           { return s.eventMux }

// Protocols implements node.Service, returning all the currently configured
// network protocols to start.
func (s *LightAtlantis) Protocols() []p2p.Protocol {
	return s.protocolManager.SubProtocols
}

// Start implements node.Service, starting all internal goroutines needed by the
// Atlantis protocol implementation.
func (s *LightAtlantis) Start(srvr *p2p.Server) error {
	s.startBloomHandlers()
	log.Warn("Light client mode is an experimental feature")
	s.netRPCService = athapi.NewPublicNetAPI(srvr, s.networkId)
	// clients are searching for the first advertised protocol in the list
	protocolVersion := AdvertiseProtocolVersions[0]
	s.serverPool.start(srvr, lesTopic(s.blockchain.Genesis().Hash(), protocolVersion))
	s.protocolManager.Start(s.config.LightPeers)
	return nil
}

// Stop implements node.Service, terminating all internal goroutines used by the
// Atlantis protocol.
func (s *LightAtlantis) Stop() error {
	s.odr.Stop()
	if s.bloomIndexer != nil {
		s.bloomIndexer.Close()
	}
	if s.chtIndexer != nil {
		s.chtIndexer.Close()
	}
	if s.bloomTrieIndexer != nil {
		s.bloomTrieIndexer.Close()
	}
	s.blockchain.Stop()
	s.protocolManager.Stop()
	s.txPool.Stop()

	s.eventMux.Stop()

	time.Sleep(time.Millisecond * 200)
	s.chainDb.Close()
	close(s.shutdownChan)

	return nil
}
