package clusters

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kubecost/cost-model/pkg/env"
	"github.com/kubecost/cost-model/pkg/log"
	"github.com/kubecost/cost-model/pkg/prom"
	"github.com/kubecost/cost-model/pkg/thanos"
	"github.com/kubecost/cost-model/pkg/util/retry"

	prometheus "github.com/prometheus/client_golang/api"
)

const (
	LoadRetries    int           = 6
	LoadRetryDelay time.Duration = 10 * time.Second
)

type ClusterInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Profile     string `json:"profile"`
	Provider    string `json:"provider"`
	Provisioner string `json:"provisioner"`
}

// Clone creates a copy of ClusterInfo and returns it
func (ci *ClusterInfo) Clone() *ClusterInfo {
	if ci == nil {
		return nil
	}

	return &ClusterInfo{
		ID:          ci.ID,
		Name:        ci.Name,
		Profile:     ci.Profile,
		Provider:    ci.Provider,
		Provisioner: ci.Provisioner,
	}
}

type ClusterMap interface {
	// GetClusterIDs returns a slice containing all of the cluster identifiers.
	GetClusterIDs() []string

	// AsMap returns the cluster map as a standard go map
	AsMap() map[string]*ClusterInfo

	// InfoFor returns the ClusterInfo entry for the provided clusterID or nil if it
	// doesn't exist
	InfoFor(clusterID string) *ClusterInfo

	// NameFor returns the name of the cluster provided the clusterID.
	NameFor(clusterID string) string

	// NameIDFor returns an identifier in the format "<clusterName>/<clusterID>" if the cluster has an
	// assigned name. Otherwise, just the clusterID is returned.
	NameIDFor(clusterID string) string

	// SplitNameID splits the nameID back into a separate id and name field
	SplitNameID(nameID string) (id string, name string)

	// StopRefresh stops the automatic internal map refresh
	StopRefresh()
}

// LocalClusterInfoProvider is a contract which is capable of performing local cluster info lookups.
type LocalClusterInfoProvider interface {
	// GetClusterInfo returns a string map containing the local cluster info
	GetClusterInfo() map[string]string
}

// ClusterMap keeps records of all known cost-model clusters.
type PrometheusClusterMap struct {
	lock         *sync.RWMutex
	client       prometheus.Client
	clusters     map[string]*ClusterInfo
	localCluster LocalClusterInfoProvider
	stop         chan struct{}
}

// NewClusterMap creates a new ClusterMap implementation using a prometheus or thanos client
func NewClusterMap(client prometheus.Client, lcip LocalClusterInfoProvider, refresh time.Duration) ClusterMap {
	stop := make(chan struct{})

	cm := &PrometheusClusterMap{
		lock:         new(sync.RWMutex),
		client:       client,
		clusters:     make(map[string]*ClusterInfo),
		localCluster: lcip,
		stop:         stop,
	}

	// Run an updater to ensure cluster data stays relevant over time
	go func() {
		// Immediately Attempt to refresh the clusters
		cm.refreshClusters()

		// Tick on interval and refresh clusters
		ticker := time.NewTicker(refresh)
		for {
			select {
			case <-ticker.C:
				cm.refreshClusters()
			case <-cm.stop:
				log.Infof("ClusterMap refresh stopped.")
				return
			}
		}
	}()

	return cm
}

// clusterInfoQuery returns the query string to load cluster info
func clusterInfoQuery(offset string) string {
	return fmt.Sprintf("kubecost_cluster_info%s", offset)
}

// loadClusters loads all the cluster info to map
func (pcm *PrometheusClusterMap) loadClusters() (map[string]*ClusterInfo, error) {
	var offset string = ""
	if prom.IsThanos(pcm.client) {
		offset = thanos.QueryOffset()
	}

	// Execute Query
	tryQuery := func() (interface{}, error) {
		ctx := prom.NewNamedContext(pcm.client, prom.ClusterMapContextName)
		r, _, e := ctx.QuerySync(clusterInfoQuery(offset))
		return r, e
	}

	// Retry on failure
	result, err := retry.Retry(context.Background(), tryQuery, uint(LoadRetries), LoadRetryDelay)

	qr, ok := result.([]*prom.QueryResult)
	if !ok || err != nil {
		return nil, err
	}

	clusters := make(map[string]*ClusterInfo)

	// Load the query results. Critical fields are id and name.
	for _, result := range qr {
		id, err := result.GetString("id")
		if err != nil {
			log.Warningf("Failed to load 'id' field for ClusterInfo")
			continue
		}

		name, err := result.GetString("name")
		if err != nil {
			log.Warningf("Failed to load 'name' field for ClusterInfo")
			continue
		}

		profile, err := result.GetString("clusterprofile")
		if err != nil {
			profile = ""
		}

		provider, err := result.GetString("provider")
		if err != nil {
			provider = ""
		}

		provisioner, err := result.GetString("provisioner")
		if err != nil {
			provisioner = ""
		}

		clusters[id] = &ClusterInfo{
			ID:          id,
			Name:        name,
			Profile:     profile,
			Provider:    provider,
			Provisioner: provisioner,
		}
	}

	// populate the local cluster if it doesn't exist
	localID := env.GetClusterID()
	if _, ok := clusters[localID]; !ok {
		localInfo, err := pcm.getLocalClusterInfo()
		if err != nil {
			log.Warningf("Failed to load local cluster info: %s", err)
		} else {
			clusters[localInfo.ID] = localInfo
		}
	}

	return clusters, nil
}

// getLocalClusterInfo returns the local cluster info in the event there does not exist a metric available.
func (pcm *PrometheusClusterMap) getLocalClusterInfo() (*ClusterInfo, error) {
	info := pcm.localCluster.GetClusterInfo()

	var id string
	var name string

	if i, ok := info["id"]; ok {
		id = i
	} else {
		return nil, fmt.Errorf("Local Cluster Info Missing ID")
	}
	if n, ok := info["name"]; ok {
		name = n
	} else {
		return nil, fmt.Errorf("Local Cluster Info Missing Name")
	}

	var clusterProfile string
	var provider string
	var provisioner string

	if cp, ok := info["clusterProfile"]; ok {
		clusterProfile = cp
	}
	if pvdr, ok := info["provider"]; ok {
		provider = pvdr
	}
	if pvsr, ok := info["provisioner"]; ok {
		provisioner = pvsr
	}

	return &ClusterInfo{
		ID:          id,
		Name:        name,
		Profile:     clusterProfile,
		Provider:    provider,
		Provisioner: provisioner,
	}, nil
}

// refreshClusters loads the clusters and updates the internal map
func (pcm *PrometheusClusterMap) refreshClusters() {
	updated, err := pcm.loadClusters()
	if err != nil {
		log.Errorf("Failed to load cluster info via query after %d retries", LoadRetries)
		return
	}

	pcm.lock.Lock()
	pcm.clusters = updated
	pcm.lock.Unlock()
}

// GetClusterIDs returns a slice containing all of the cluster identifiers.
func (pcm *PrometheusClusterMap) GetClusterIDs() []string {
	pcm.lock.RLock()
	defer pcm.lock.RUnlock()

	var clusterIDs []string
	for id := range pcm.clusters {
		clusterIDs = append(clusterIDs, id)
	}

	return clusterIDs
}

// AsMap returns the cluster map as a standard go map
func (pcm *PrometheusClusterMap) AsMap() map[string]*ClusterInfo {
	pcm.lock.RLock()
	defer pcm.lock.RUnlock()

	m := make(map[string]*ClusterInfo)
	for k, v := range pcm.clusters {
		m[k] = v.Clone()
	}

	return m
}

// InfoFor returns the ClusterInfo entry for the provided clusterID or nil if it
// doesn't exist
func (pcm *PrometheusClusterMap) InfoFor(clusterID string) *ClusterInfo {
	pcm.lock.RLock()
	defer pcm.lock.RUnlock()

	if info, ok := pcm.clusters[clusterID]; ok {
		return info.Clone()
	}

	return nil
}

// NameFor returns the name of the cluster provided the clusterID.
func (pcm *PrometheusClusterMap) NameFor(clusterID string) string {
	pcm.lock.RLock()
	defer pcm.lock.RUnlock()

	if info, ok := pcm.clusters[clusterID]; ok {
		return info.Name
	}

	return ""
}

// NameIDFor returns an identifier in the format "<clusterName>/<clusterID>" if the cluster has an
// assigned name. Otherwise, just the clusterID is returned.
func (pcm *PrometheusClusterMap) NameIDFor(clusterID string) string {
	pcm.lock.RLock()
	defer pcm.lock.RUnlock()

	if info, ok := pcm.clusters[clusterID]; ok {
		if info.Name == "" {
			return clusterID
		}

		return fmt.Sprintf("%s/%s", info.Name, clusterID)
	}

	return clusterID
}

func (pcm *PrometheusClusterMap) SplitNameID(nameID string) (id string, name string) {
	if !strings.Contains(nameID, "/") {
		id = nameID
		name = ""
		return
	}

	split := strings.Split(nameID, "/")
	name = split[0]
	id = split[1]
	return
}

// StopRefresh stops the automatic internal map refresh
func (pcm *PrometheusClusterMap) StopRefresh() {
	if pcm.stop != nil {
		close(pcm.stop)
		pcm.stop = nil
	}
}
