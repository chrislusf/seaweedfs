package wdclient

import (
	"context"
	"fmt"
	"time"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
	"github.com/chrislusf/seaweedfs/weed/util"
	"math/rand"
)

type MasterClient struct {
	ctx           context.Context
	name          string
	currentMaster string
	masters       []string

	vidMap
}

func NewMasterClient(ctx context.Context, clientName string, masters []string) *MasterClient {
	return &MasterClient{
		ctx:     ctx,
		name:    clientName,
		masters: masters,
		vidMap:  newVidMap(),
	}
}

func (mc *MasterClient) GetMaster() string {
	return mc.currentMaster
}

func (mc *MasterClient) WaitUntilConnected() {
	for mc.currentMaster == "" {
		time.Sleep(time.Duration(rand.Int31n(200)) * time.Millisecond)
	}
}

func (mc *MasterClient) KeepConnectedToMaster() {
	glog.V(0).Infof("%s bootstraps with masters %v", mc.name, mc.masters)
	for {
		mc.tryAllMasters()
		time.Sleep(time.Second)
	}
}

func (mc *MasterClient) tryAllMasters() {
	for _, master := range mc.masters {
		glog.V(0).Infof("Connecting to master %v", master)
		gprcErr := withMasterClient(master, func(client master_pb.SeaweedClient) error {

			stream, err := client.KeepConnected(context.Background())
			if err != nil {
				glog.V(0).Infof("failed to keep connected to %s: %v", master, err)
				return err
			}

			if err = stream.Send(&master_pb.ClientListenRequest{Name: mc.name}); err != nil {
				glog.V(0).Infof("failed to send to %s: %v", master, err)
				return err
			}

			if mc.currentMaster == "" {
				glog.V(0).Infof("Connected to %v", master)
				mc.currentMaster = master
			}

			for {
				if volumeLocation, err := stream.Recv(); err != nil {
					glog.V(0).Infof("failed to receive from %s: %v", master, err)
					return err
				} else {
					loc := Location{
						Url:       volumeLocation.Url,
						PublicUrl: volumeLocation.PublicUrl,
					}
					for _, newVid := range volumeLocation.NewVids {
						mc.addLocation(newVid, loc)
					}
					for _, deletedVid := range volumeLocation.DeletedVids {
						mc.deleteLocation(deletedVid, loc)
					}
				}
			}

		})

		if gprcErr != nil {
			glog.V(0).Infof("%s failed to connect with master %v: %v", mc.name, master, gprcErr)
		}

		mc.currentMaster = ""
	}
}

func withMasterClient(master string, fn func(client master_pb.SeaweedClient) error) error {

	masterGrpcAddress, parseErr := util.ParseServerToGrpcAddress(master, 0)
	if parseErr != nil {
		return fmt.Errorf("failed to parse master grpc %v", master)
	}

	grpcConnection, err := util.GrpcDial(masterGrpcAddress)
	if err != nil {
		return fmt.Errorf("fail to dial %s: %v", master, err)
	}
	defer grpcConnection.Close()

	client := master_pb.NewSeaweedClient(grpcConnection)

	return fn(client)
}
