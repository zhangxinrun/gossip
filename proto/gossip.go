package proto

import (
	"errors"
	log "github.com/Sirupsen/logrus"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/libopenstorage/gossip/api"
)

const (
	// XXX/gsangle: Should these come from some sort of config file?
	CONN_HOST = "0.0.0.0"
	CONN_PORT = "9002"
	CONN_TYPE = "tcp"

	// interval to gossip, may be should make it configurable ?
	GOSSIP_INTERVAL = 2 * time.Minute
)

func connectionString(ip string) string {
	if strings.Index(ip, ":") == -1 {
		return ip + ":" + CONN_PORT
	}
	return ip
}

// Implements the UnreliableBroadcast interface
type GossiperImpl struct {
	// GossipstoreImpl implements the GossipStoreInterface
	GossipStoreImpl

	// node list, maintained separately
	nodes     []string
	name      string
	nodesLock sync.Mutex
	// to signal exit gossip loop
	done           chan bool
	gossipInterval time.Duration

	// the actual in-memory state
	store api.GossipStore
}

// Utility methods
func logAndGetError(msg string) error {
	log.Error(msg)
	return errors.New(msg)
}

// New returns an initialized Gossip node
// which identifies itself with the given ip
func NewGossiper(ip string, selfNodeId api.NodeId) api.Gossiper {
	return new(GossiperImpl).init(ip, selfNodeId)
}

func (g *GossiperImpl) init(ip string, selfNodeId api.NodeId) api.Gossiper {
	g.id = selfNodeId
	g.name = ip
	g.nodes = make([]string, 10) // random initial capacity
	g.done = make(chan bool, 1)
	rand.Seed(time.Now().UnixNano())
	err := g.AddNode(ip)
	if err != nil {
		log.Error("Failed to add init node to store")
		return nil
	}
	return g
}

func (g *GossiperImpl) Stop() {
	g.done <- true
}

func (g *GossiperImpl) SetGossipInterval(t time.Duration) {
	g.gossipInterval = t
}

func (g *GossiperImpl) GossipInterval() time.Duration {
	return g.gossipInterval
}

func (g *GossiperImpl) AddNode(ip string) error {
	g.nodesLock.Lock()
	defer g.nodesLock.Unlock()

	for _, node := range g.nodes {
		if node == ip {
			return logAndGetError("Node being added already exists:" + ip)
		}
	}
	g.nodes = append(g.nodes, ip)

	return nil
}

func (g *GossiperImpl) RemoveNode(ip string) error {
	g.nodesLock.Lock()
	defer g.nodesLock.Unlock()

	for i, node := range g.nodes {
		if node == ip {
			// not sure if this is the most efficient way
			g.nodes = append(g.nodes[:i], g.nodes[i+1:]...)
			return nil
		}
	}
	return logAndGetError("Node being added already exists:" + ip)
}

func (g *GossiperImpl) GetNodes() []string {
	g.nodesLock.Lock()
	defer g.nodesLock.Unlock()

	nodeList := make([]string, len(g.nodes))
	copy(nodeList, g.nodes)
	return nodeList
}

// getUpdatesFromPeer receives node data from the peer
// for which the peer has more latest information available
func (g *GossiperImpl) getUpdatesFromPeer(conn net.Conn) error {

	var newPeerData api.StoreDiff
	err := rcvData(&newPeerData, conn)
	if err != nil {
		log.Error("Error fetching the latest peer data", err)
		return err
	}

	g.store.Update(newPeerData)

	return nil
}

// sendNodeMetaInfo sends a list of meta info for all
// the nodes in the nodes's store to the peer
func (g *GossiperImpl) sendNodeMetaInfo(conn net.Conn) error {
	msg := g.store.MetaInfo()
	err := sendData(msg, conn)
	return err
}

// sendUpdatesToPeer sends the information about the given
// nodes to the peer
func (g *GossiperImpl) sendUpdatesToPeer(diff *api.StoreNodes, conn net.Conn) error {
	dataToSend := g.store.Subset(*diff)
	return sendData(dataToSend, conn)
}

func (g *GossiperImpl) handleGossip(conn net.Conn) {
	var peerMetaInfo api.StoreMetaInfo
	err := error(nil)

	// 1. Get the info about the node data that the sender has
	// XXX FIXME : readPeerData must be passed using a pointer
	err = rcvData(&peerMetaInfo, conn)
	if err != nil {
		return
	}

	// 2. Compare with current data that this node has and get
	//    the names of the nodes for which this node has stale info
	//    as compared to the sender
	diffNew, selfNew := g.store.Diff(peerMetaInfo)

	// 3. Send this list to the peer, and get the latest data
	// for them
	err = sendData(diffNew, conn)
	if err != nil {
		log.Error("Error sending list of nodes to fetch: ", err)
		return
	}

	// 4. get the data for nodes sent above from the peer
	err = g.getUpdatesFromPeer(conn)
	if err != nil {
		log.Error("Failed to get data for nodes from the peer: ", err)
		return
	}

	// 4. Since you know which data is stale on the sender side,
	//    send him the data for the updated nodes
	err = g.sendUpdatesToPeer(&selfNew, conn)
	if err != nil {
		return
	}
}

func (g *GossiperImpl) receive_loop() {
	l, err := net.Listen(CONN_TYPE, CONN_HOST+":"+CONN_PORT)
	if err != nil {
		log.Println("Error listening:", err.Error())
		os.Exit(1)
	}
	defer l.Close()

	log.Println("Listening on " + CONN_HOST + ":" + CONN_PORT)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		// Handle connections in a new goroutine.
		go g.handleGossip(conn)
	}
}

// send_loop periodically connects to a random peer
// and gossips about the state of the cluster
func (g *GossiperImpl) send_loop() {
	tick := time.Tick(GOSSIP_INTERVAL)
	for {
		select {
		case <-tick:
			g.gossip()
		case <-g.done:
			log.Info("send_loop now exiting")
		default:
			log.Error("send_loop default!")
		}
	}

}

// selectGossipPeer randomly selects a peer
// to gossip with from the list of nodes added
func (g *GossiperImpl) selectGossipPeer() string {
	g.nodesLock.Lock()
	defer g.nodesLock.Unlock()

	nodesLen := len(g.nodes)
	if nodesLen != 0 {
		log.Info("No peers to gossip with, returning")
		return ""
	}

	return g.nodes[rand.Intn(nodesLen)]
}

func (g *GossiperImpl) gossip() {

	// select a node to gossip with
	peerNode := g.selectGossipPeer()
	if len(peerNode) == 0 {
		return
	}

	conn, err := net.Dial(CONN_TYPE, connectionString(peerNode))
	if err != nil {
		log.Error("Peer " + peerNode + " unavailable to gossip")
		//XXX: FIXME : note that the peer is down
		return
	}

	// send meta data info about the node to the peer
	err = g.sendNodeMetaInfo(conn)
	if err != nil {
		log.Error("Failed to send meta info to the peer: ", err)
		//XXX: FIXME : note that the peer is down
		return
	}

	// get a list of requested nodes from the peer and
	var diff api.StoreNodes
	err = rcvData(&diff, conn)
	if err != nil {
		log.Error("Failed to get request info to the peer: ", err)
		//XXX: FIXME : note that the peer is down
		return
	}

	// send back the data
	err = g.sendUpdatesToPeer(&diff, conn)
	if err != nil {
		log.Error("Failed to send newer data to the peer: ", err)
		//XXX: FIXME : note that the peer is down
		return
	}

	// receive any updates the send has for us
	err = g.getUpdatesFromPeer(conn)
	if err != nil {
		log.Error("Failed to get newer data from the peer: ", err)
		//XXX: FIXME : note that the peer is down
		return
	}

}
