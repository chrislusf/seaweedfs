package weed_server

import (
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/chrislusf/weed-fs/go/glog"
	"github.com/chrislusf/weed-fs/go/security"
	"github.com/chrislusf/weed-fs/go/storage"
)

type VolumeServer struct {
	masterNode   string
	mnLock       sync.RWMutex
	pulseSeconds int
	dataCenter   string
	rack         string
	store        *storage.Store
	guard        *security.Guard

	FixJpgOrientation bool
}

func NewVolumeServer(publicMux, adminMux *http.ServeMux, ip string,
	port, adminPort int, publicIp string,
	folders []string, maxCounts []int,
	masterNode string, pulseSeconds int,
	dataCenter string, rack string,
	whiteList []string,
	fixJpgOrientation bool) *VolumeServer {
	publicUrl := publicIp + ":" + strconv.Itoa(port)
	vs := &VolumeServer{
		pulseSeconds:      pulseSeconds,
		dataCenter:        dataCenter,
		rack:              rack,
		FixJpgOrientation: fixJpgOrientation,
	}
	vs.SetMasterNode(masterNode)
	vs.store = storage.NewStore(port, adminPort, ip, publicUrl, folders, maxCounts)

	vs.guard = security.NewGuard(whiteList, "")

	adminMux.HandleFunc("/status", vs.guard.Secure(vs.statusHandler))
	adminMux.HandleFunc("/admin/assign_volume", vs.guard.Secure(vs.assignVolumeHandler))
	adminMux.HandleFunc("/admin/vacuum_volume_check", vs.guard.Secure(vs.vacuumVolumeCheckHandler))
	adminMux.HandleFunc("/admin/vacuum_volume_compact", vs.guard.Secure(vs.vacuumVolumeCompactHandler))
	adminMux.HandleFunc("/admin/vacuum_volume_commit", vs.guard.Secure(vs.vacuumVolumeCommitHandler))
	adminMux.HandleFunc("/admin/freeze_volume", vs.guard.Secure(vs.freezeVolumeHandler))
	adminMux.HandleFunc("/admin/delete_collection", vs.guard.Secure(vs.deleteCollectionHandler))
	adminMux.HandleFunc("/stats/counter", vs.guard.Secure(statsCounterHandler))
	adminMux.HandleFunc("/stats/memory", vs.guard.Secure(statsMemoryHandler))
	adminMux.HandleFunc("/stats/disk", vs.guard.Secure(vs.statsDiskHandler))
	publicMux.HandleFunc("/delete", vs.guard.Secure(vs.batchDeleteHandler))
	publicMux.HandleFunc("/", vs.storeHandler)

	go func() {
		connected := true

		vs.store.SetBootstrapMaster(vs.GetMasterNode())
		vs.store.SetDataCenter(vs.dataCenter)
		vs.store.SetRack(vs.rack)
		for {
			master, err := vs.store.Join()
			if err == nil {
				if !connected {
					connected = true
					vs.SetMasterNode(master)
					glog.V(0).Infoln("Volume Server Connected with master at", master, "and set it as masterNode")
				}
			} else {
				glog.V(4).Infoln("Volume Server Failed to talk with master:", err.Error())
				if connected {
					connected = false
				}
			}
			if connected {
				time.Sleep(time.Duration(float32(vs.pulseSeconds*1e3)*(1+rand.Float32())) * time.Millisecond)
			} else {
				time.Sleep(time.Duration(float32(vs.pulseSeconds*1e3)*0.25) * time.Millisecond)
			}
		}
	}()

	return vs
}

func (vs *VolumeServer) GetMasterNode() string {
	vs.mnLock.RLock()
	defer vs.mnLock.RUnlock()
	return vs.masterNode
}

func (vs *VolumeServer) SetMasterNode(masterNode string) {
	vs.mnLock.Lock()
	defer vs.mnLock.Unlock()
	vs.masterNode = masterNode
}

func (vs *VolumeServer) Shutdown() {
	glog.V(0).Infoln("Shutting down volume server...")
	vs.store.Close()
	glog.V(0).Infoln("Shut down successfully!")
}
