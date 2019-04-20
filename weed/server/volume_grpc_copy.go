package weed_server

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/operation"
	"github.com/chrislusf/seaweedfs/weed/pb/volume_server_pb"
	"github.com/chrislusf/seaweedfs/weed/storage"
	"github.com/chrislusf/seaweedfs/weed/storage/needle"
)

// VolumeCopy copy the .idx .dat files, and mount the volume
func (vs *VolumeServer) VolumeCopy(ctx context.Context, req *volume_server_pb.VolumeCopyRequest) (*volume_server_pb.VolumeCopyResponse, error) {

	v := vs.store.GetVolume(needle.VolumeId(req.VolumeId))
	if v != nil {
		// unmount the volume
		err := vs.store.UnmountVolume(needle.VolumeId(req.VolumeId))
		if err != nil {
			return nil, fmt.Errorf("failed to unmount volume %d: %v", req.VolumeId, err)
		}
	}

	location := vs.store.FindFreeLocation()
	if location == nil {
		return nil, fmt.Errorf("no space left")
	}

	volumeFileName := storage.VolumeFileName(req.Collection, location.Directory, int(req.VolumeId))

	// the master will not start compaction for read-only volumes, so it is safe to just copy files directly
	// copy .dat and .idx files
	//   read .idx .dat file size and timestamp
	//   send .idx file
	//   send .dat file
	//   confirm size and timestamp
	var volFileInfoResp *volume_server_pb.ReadVolumeFileStatusResponse
	datFileName := volumeFileName + ".dat"
	idxFileName := volumeFileName + ".idx"
	err := operation.WithVolumeServerClient(req.SourceDataNode, vs.grpcDialOption, func(client volume_server_pb.VolumeServerClient) error {
		var err error
		volFileInfoResp, err = client.ReadVolumeFileStatus(ctx,
			&volume_server_pb.ReadVolumeFileStatusRequest{
				VolumeId: req.VolumeId,
			})
		if nil != err {
			return fmt.Errorf("read volume file status failed, %v", err)
		}

		// println("source:", volFileInfoResp.String())

		copyFileClient, err := client.CopyFile(ctx, &volume_server_pb.CopyFileRequest{
			VolumeId:           req.VolumeId,
			IsIdxFile:          true,
			CompactionRevision: volFileInfoResp.CompactionRevision,
			StopOffset:         volFileInfoResp.IdxFileSize,
		})
		if err != nil {
			return fmt.Errorf("failed to start copying volume %d idx file: %v", req.VolumeId, err)
		}

		err = writeToFile(copyFileClient, idxFileName)
		if err != nil {
			return fmt.Errorf("failed to copy volume %d idx file: %v", req.VolumeId, err)
		}

		copyFileClient, err = client.CopyFile(ctx, &volume_server_pb.CopyFileRequest{
			VolumeId:           req.VolumeId,
			IsDatFile:          true,
			CompactionRevision: volFileInfoResp.CompactionRevision,
			StopOffset:         volFileInfoResp.DatFileSize,
		})
		if err != nil {
			return fmt.Errorf("failed to start copying volume %d dat file: %v", req.VolumeId, err)
		}

		err = writeToFile(copyFileClient, datFileName)
		if err != nil {
			return fmt.Errorf("failed to copy volume %d dat file: %v", req.VolumeId, err)
		}

		return nil
	})
	if err != nil {
		os.Remove(idxFileName)
		os.Remove(datFileName)
		return nil, err
	}

	if err = checkCopyFiles(volFileInfoResp, idxFileName, datFileName); err != nil {
		return nil, err
	}

	// mount the volume
	err = vs.store.MountVolume(needle.VolumeId(req.VolumeId))
	if err != nil {
		return nil, fmt.Errorf("failed to mount volume %d: %v", req.VolumeId, err)
	}

	return &volume_server_pb.VolumeCopyResponse{
		LastAppendAtNs: volFileInfoResp.DatFileTimestampSeconds * uint64(time.Second),
	}, err
}

/**
only check the the differ of the file size
todo: maybe should check the received count and deleted count of the volume
*/
func checkCopyFiles(originFileInf *volume_server_pb.ReadVolumeFileStatusResponse, idxFileName, datFileName string) error {
	stat, err := os.Stat(idxFileName)
	if err != nil {
		return fmt.Errorf("get idx file info failed, %v", err)
	}
	if originFileInf.IdxFileSize != uint64(stat.Size()) {
		return fmt.Errorf("the idx file size [%v] is not same as origin file size [%v]",
			stat.Size(), originFileInf.IdxFileSize)
	}

	stat, err = os.Stat(datFileName)
	if err != nil {
		return fmt.Errorf("get dat file info failed, %v", err)
	}
	if originFileInf.DatFileSize != uint64(stat.Size()) {
		return fmt.Errorf("the dat file size [%v] is not same as origin file size [%v]",
			stat.Size(), originFileInf.DatFileSize)
	}
	return nil
}

func writeToFile(client volume_server_pb.VolumeServer_CopyFileClient, fileName string) error {
	glog.V(4).Infof("writing to %s", fileName)
	dst, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil
	}
	defer dst.Close()

	for {
		resp, receiveErr := client.Recv()
		if receiveErr == io.EOF {
			break
		}
		if receiveErr != nil {
			return fmt.Errorf("receiving %s: %v", fileName, receiveErr)
		}
		dst.Write(resp.FileContent)
	}
	return nil
}

func (vs *VolumeServer) ReadVolumeFileStatus(ctx context.Context, req *volume_server_pb.ReadVolumeFileStatusRequest) (*volume_server_pb.ReadVolumeFileStatusResponse, error) {
	resp := &volume_server_pb.ReadVolumeFileStatusResponse{}
	v := vs.store.GetVolume(needle.VolumeId(req.VolumeId))
	if v == nil {
		return nil, fmt.Errorf("not found volume id %d", req.VolumeId)
	}

	resp.VolumeId = req.VolumeId
	datSize, idxSize, modTime := v.FileStat()
	resp.DatFileSize = datSize
	resp.IdxFileSize = idxSize
	resp.DatFileTimestampSeconds = uint64(modTime.Unix())
	resp.IdxFileTimestampSeconds = uint64(modTime.Unix())
	resp.FileCount = v.FileCount()
	resp.CompactionRevision = uint32(v.CompactionRevision)
	return resp, nil
}

func (vs *VolumeServer) CopyFile(req *volume_server_pb.CopyFileRequest, stream volume_server_pb.VolumeServer_CopyFileServer) error {

	v := vs.store.GetVolume(needle.VolumeId(req.VolumeId))
	if v == nil {
		return fmt.Errorf("not found volume id %d", req.VolumeId)
	}

	if uint32(v.CompactionRevision) != req.CompactionRevision {
		return fmt.Errorf("volume %d is compacted", req.VolumeId)
	}

	bytesToRead := int64(req.StopOffset)

	const BufferSize = 1024 * 1024 * 2
	var fileName = v.FileName()
	if req.IsDatFile {
		fileName += ".dat"
	} else if req.IsIdxFile {
		fileName += ".idx"
	}
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	buffer := make([]byte, BufferSize)

	for bytesToRead > 0 {
		bytesread, err := file.Read(buffer)

		// println(fileName, "read", bytesread, "bytes, with target", bytesToRead)

		if err != nil {
			if err != io.EOF {
				return err
			}
			// println(fileName, "read", bytesread, "bytes, with target", bytesToRead, "err", err.Error())
			break
		}

		if int64(bytesread) > bytesToRead {
			bytesread = int(bytesToRead)
		}
		err = stream.Send(&volume_server_pb.CopyFileResponse{
			FileContent: buffer[:bytesread],
		})
		if err != nil {
			// println("sending", bytesread, "bytes err", err.Error())
			return err
		}

		bytesToRead -= int64(bytesread)

	}

	return nil
}
