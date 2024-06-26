package clustmgr

import (
	"fmt"
	"github.com/spirit-labs/tektite/common"
	log "github.com/spirit-labs/tektite/logger"
	"math"
	"sort"
	"sync"
	"time"
)

const panicOnDataLoss = false

type StateManager interface {
	Start() error
	Stop() error
	Halt() error
	Freeze()
	SetClusterStateHandler(handler ClusterStateHandler)
	MarkGroupAsValid(nodeID int, groupID int, joinedVersion int) (bool, error)
	PrepareForShutdown()
}

func NewClusteredStateManager(keyPrefix string, clusterName string, nodeID int, endpoints []string, leaseTime time.Duration,
	sendUpdatePeriod time.Duration, callTimeout time.Duration, numGroups int, maxReplicas int) *ClusteredStateManager {
	sm := &ClusteredStateManager{
		nodeID:             nodeID,
		numGroups:          numGroups,
		maxReplicas:        maxReplicas,
		calcGroupStateChan: make(chan map[int]int64, 100),
	}
	client := NewClient(keyPrefix, clusterName, nodeID, endpoints, leaseTime, sendUpdatePeriod, callTimeout, sm.setNodesState,
		sm.setClusterState)
	sm.client = client
	return sm
}

type ClusteredStateManager struct {
	lock                sync.Mutex
	started             bool
	stopped             bool
	clusterLeader       bool
	nodeID              int
	clusterStateHandler ClusterStateHandler
	numGroups           int
	maxReplicas         int
	client              Client
	calcGroupStateChan  chan map[int]int64
	frozen              bool
}

func (sm *ClusteredStateManager) Freeze() {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	sm.frozen = true
}

func (sm *ClusteredStateManager) Start() error {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	if sm.started {
		panic("state manager already started")
	}
	if sm.stopped {
		panic("cannot be restarted")
	}
	if sm.clusterStateHandler == nil {
		panic("clusterStateHandler not set")
	}
	common.Go(sm.calcAndSetSetLoop)
	sm.started = true
	return sm.client.Start()
}

func (sm *ClusteredStateManager) Stop() error {
	return sm.stop(false)
}

func (sm *ClusteredStateManager) Halt() error {
	return sm.stop(true)
}

func (sm *ClusteredStateManager) PrepareForShutdown() {
	sm.client.PrepareForShutdown()
}

func (sm *ClusteredStateManager) stop(halt bool) error {
	if err := sm.client.Stop(halt); err != nil {
		log.Warnf("failed to stop clust mgr client %v", err)
	}
	sm.lock.Lock()
	defer sm.lock.Unlock()
	if sm.stopped {
		return nil
	}
	close(sm.calcGroupStateChan)
	sm.stopped = true
	return nil
}

func (sm *ClusteredStateManager) SetClusterStateHandler(handler ClusterStateHandler) {
	sm.clusterStateHandler = handler
}

func (sm *ClusteredStateManager) MarkGroupAsValid(nodeID int, groupID int, joinedVersion int) (bool, error) {
	return sm.client.MarkGroupAsValid(nodeID, groupID, joinedVersion)
}

func (sm *ClusteredStateManager) setClusterState(clusterState ClusterState) {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	if sm.stopped || sm.frozen {
		return
	}
	if err := sm.clusterStateHandler(clusterState); err != nil {
		log.Errorf("failed to handle cluster state %v", err)
	}
}

func (sm *ClusteredStateManager) calcAndSetSetLoop() {
	for activeNodes := range sm.calcGroupStateChan {
		sm.handleActiveNodes(activeNodes)
	}
}

func (sm *ClusteredStateManager) setNodesState(activeNodes map[int]int64) {
	sm.calcGroupStateChan <- activeNodes
}

func (sm *ClusteredStateManager) handleActiveNodes(activeNodes map[int]int64) {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	if sm.stopped || sm.frozen {
		return
	}
	if len(activeNodes) == 0 {
		return
	}
	var groupStates [][]GroupNode

	var nids []int
	for nid := range activeNodes {
		nids = append(nids, nid)
	}
	sort.Ints(nids)

	leaderNode := nids[0] // We just choose the first one in the sorted slice
	if leaderNode == sm.nodeID {
		// We are the cluster leader
		// Get the current state - the new state depends on that
		cs, ns, ver, err := sm.client.GetClusterState()
		if err != nil {
			log.Errorf("failed to get cluster state %v", err)
			return
		}
		if cs != nil {
			// Get the valid groups state
			validGroups, err := sm.client.GetValidGroups()
			if err != nil {
				log.Errorf("failed to get valid groups %v", err)
				return
			}
			// Update whether the replicas are valid or not
			for groupID, groupState := range cs.GroupStates {
				for i := 0; i < len(groupState); i++ {
					gn := &groupState[i]
					if !gn.Leader {
						key := fmt.Sprintf("%d/%d", gn.NodeID, groupID)
						markedAsValidVer, ok := validGroups[key]
						// if the valid group joined version is not equal to joined version of the group in the existing cluster
						// state then we don't mark it as valid as this is from an old replica
						if ok && markedAsValidVer == gn.JoinedVersion {
							gn.Valid = true
						} else {
							gn.Valid = false
						}
					}
				}
			}
		}

		// We keep track of whether nodes are added by looking at the version
		// this handles the case where a node is quickly bounced and is in both current and previous
		// node states, but with a different version
		activeNodesWithAdded := make(map[int]bool, len(activeNodes))
		for nid, ver := range activeNodes {
			prevVer, ok := ns[nid]
			if !ok || prevVer != ver {
				// It's an added node
				activeNodesWithAdded[nid] = true
			} else {
				activeNodesWithAdded[nid] = false
			}
		}

		var newClusterVer int
		if cs != nil {
			newClusterVer = cs.Version + 1
		}
		var ok bool
		ok, groupStates = calculateGroupStates(cs, activeNodesWithAdded, sm.numGroups, sm.maxReplicas, newClusterVer)
		if !ok {
			msg := fmt.Sprintf("no valid nodes from previous cluster state remain. this could be due to a previous cluster crash.")
			log.Warnf(msg)
			if panicOnDataLoss {
				panic(msg)
			}
		}
		newState := &ClusterState{
			Version:     newClusterVer,
			GroupStates: groupStates,
		}
		verifyStateBalanced(newState)
		ok, err = sm.client.SetClusterState(newState, activeNodes, ver)
		if err != nil {
			log.Errorf("failed to setClusterState %v", err)
			return
		}
		if !ok {
			// cluster state wasn't at expected revision so failed to set - could be multiple nodes started with
			// the same id?
			log.Warn("failed to set cluster state, version does not match")
		}
	}
}

func verifyStateBalanced(cs *ClusterState) {
	// Make sure the state is balanced - i.e. we don't have multiple replicas for the same group on the same node
	for _, groupState := range cs.GroupStates {
		nids := make(map[int]struct{}, len(groupState))
		for _, groupNode := range groupState {
			_, exists := nids[groupNode.NodeID]
			if exists {
				panic("multiple replicas on single node")
			}
			nids[groupNode.NodeID] = struct{}{}
		}
	}
}

func calculateGroupStates(currState *ClusterState, activeNodes map[int]bool, numGroups int, maxReplicas int,
	newVersion int) (bool, [][]GroupNode) {
	numActiveNodes := len(activeNodes)
	var numReplicas int
	if numActiveNodes > maxReplicas {
		numReplicas = maxReplicas
	} else {
		numReplicas = numActiveNodes
	}
	var groupStates [][]GroupNode
	replicasPerNode := map[int]int{}
	leadersPerNode := map[int]int{}
	var newLeaderStates [][]GroupNode
	for groupID := 0; groupID < numGroups; groupID++ {
		groupState := make([]GroupNode, 0, numReplicas)
		chosenNodes := map[int]struct{}{}
		if currState != nil && len(currState.GroupStates) == numGroups {
			prevGroup := currState.GroupStates[groupID]

			// We try and preserve nodes as much as possible - so if nodes still exist in the active nodes then we
			// keep the replica
			chooseLeader := true
			for _, prevGroupNode := range prevGroup {
				added, exists := activeNodes[prevGroupNode.NodeID]
				if exists && !added {
					// Note: The node might exist in previous state but added = true which means it was bounced so we
					// don't preserve it
					groupState = append(groupState, GroupNode{
						NodeID:        prevGroupNode.NodeID,
						Leader:        prevGroupNode.Leader,
						Valid:         prevGroupNode.Valid,
						JoinedVersion: prevGroupNode.JoinedVersion,
					})
					chosenNodes[prevGroupNode.NodeID] = struct{}{}
					replicasPerNode[prevGroupNode.NodeID]++
					if prevGroupNode.Leader && prevGroupNode.Valid {
						// preserve the leader
						// note, the leader can be invalid in the case that a node left the cluster then rejoined
						// quickly and marked group node as invalid (in nodeStarted) - in this case we want to choose
						// a new leader as the old one does not have the data
						leadersPerNode[prevGroupNode.NodeID]++
						chooseLeader = false
					}
				}
			}

			if len(groupState) < numReplicas {
				// We need to choose more replica(s) that have not already been chosen
				availableNodes := map[int]struct{}{}
				// First calculate which active nodes have not already got replicas for the group
				for active := range activeNodes {
					_, chosenAlready := chosenNodes[active]
					if !chosenAlready {
						availableNodes[active] = struct{}{}
					}
				}
				numToChoose := numReplicas - len(groupState)
				// Now we need to chose numToChoose from this set of available nodes
				// We choose the ones which have the fewest replicas on them already
				groupState = chooseNodes(replicasPerNode, numToChoose, availableNodes, groupState, newVersion)
			}
			if chooseLeader {
				newLeaderStates = append(newLeaderStates, groupState)
			}
		} else {
			// Choose the nodes
			availableNodes := map[int]struct{}{}
			for node := range activeNodes {
				availableNodes[node] = struct{}{}
			}
			groupState = chooseNodes(replicasPerNode, numReplicas, availableNodes, groupState, newVersion)
			newLeaderStates = append(newLeaderStates, groupState)
		}
		groupStates = append(groupStates, groupState)
	}
	// Now we need to choose the leaders for any groups, this needs to be done after all the existing leader
	// groups have been iterated through to ensure a good distribution
	validCluster := true
	for _, groupState := range newLeaderStates {
		chooseFromInvalid := false
		if currState == nil || currState.GroupStates == nil {
			// No previous state - all new nodes will be invalid, so ok to choose leader from invalid set
			chooseFromInvalid = true
		} else {
			chooseFromInvalid = true
			// If at least one replica is valid, we won't choose a leader from the invalid set
			for _, groupNode := range groupState {
				if groupNode.Valid {
					chooseFromInvalid = false
				}
			}
			if chooseFromInvalid {
				// Potential data-loss - there wasn't a valid replica to choose a leader from
				validCluster = false
			}
		}
		chooseLeaderNode(leadersPerNode, groupState, chooseFromInvalid)
	}
	return validCluster, groupStates
}

func chooseNodes(replicasPerNode map[int]int, numToChoose int, availableNodes map[int]struct{},
	groupNodes []GroupNode, newClusterVersion int) []GroupNode {
	// Now we need to chose numToChoose from this set of available nodes
	// We choose the ones which have the fewest replicas on them already
	for i := 0; i < numToChoose; i++ {
		chosen := chooseNode(replicasPerNode, availableNodes)
		groupNodes = append(groupNodes, GroupNode{
			NodeID:        chosen,
			JoinedVersion: newClusterVersion,
			Valid:         false,
		})
		replicasPerNode[chosen]++
		delete(availableNodes, chosen)
	}
	return groupNodes
}

func chooseLeaderNode(leadersPerNode map[int]int, groupNodes []GroupNode, allowInvalid bool) {
	log.Debugf("choosing a leader node, allow invalid is %t", allowInvalid)
	availableLeaderNodes := map[int]struct{}{}
	var aln []int
	for _, groupNode := range groupNodes {
		if allowInvalid || groupNode.Valid {
			availableLeaderNodes[groupNode.NodeID] = struct{}{}
			aln = append(aln, groupNode.NodeID)
		}
	}

	log.Debugf("choosing a leader from nodes %v", aln)
	if len(availableLeaderNodes) == 0 {
		panic("no available nodes to choose leader")
	}
	leader := chooseNode(leadersPerNode, availableLeaderNodes)
	found := false
	for i := 0; i < len(groupNodes); i++ {
		if groupNodes[i].NodeID == leader {
			groupNodes[i].Leader = true
			groupNodes[i].Valid = true // leaders are always valid
			found = true
		} else {
			groupNodes[i].Leader = false
		}
	}
	if !found {
		// Sanity check
		panic("cannot find leader node")
	}
	leadersPerNode[leader]++
}

func nodesToOrderedSlice(nodes map[int]struct{}) []int {
	// Note the transformation to a slice MUST be deterministic so the same result is calculated on each replica
	nodeSlice := make([]int, 0, len(nodes))
	for nid := range nodes {
		nodeSlice = append(nodeSlice, nid)
	}
	sort.Ints(nodeSlice)
	return nodeSlice
}

func chooseNode(perNodeMap map[int]int, availableNodes map[int]struct{}) int {
	nodeSlice := nodesToOrderedSlice(availableNodes)
	// We choose the first node with the lowest count
	min := math.MaxInt
	var chosen int
	for _, nid := range nodeSlice {
		count := perNodeMap[nid]
		if count < min {
			min = count
			chosen = nid
		}
	}
	return chosen
}

func (sm *ClusteredStateManager) GetClient() Client {
	return sm.client
}
