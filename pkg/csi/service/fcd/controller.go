/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fcd

import (
	"fmt"
	"strconv"
	"time"

	"golang.org/x/net/context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"

	"github.com/vmware/govmomi/units"
	"github.com/vmware/govmomi/vim25/types"

	vcfg "k8s.io/cloud-provider-vsphere/pkg/common/config"
	cm "k8s.io/cloud-provider-vsphere/pkg/common/connectionmanager"
	k8s "k8s.io/cloud-provider-vsphere/pkg/common/kubernetes"
	"k8s.io/cloud-provider-vsphere/pkg/common/vclib"
	vTypes "k8s.io/cloud-provider-vsphere/pkg/csi/types"
)

type controller struct {
	client    *clientset.Interface
	cfg       *vcfg.Config
	connMgr   *cm.ConnectionManager
	informMgr *k8s.InformerManager
}

func noResyncPeriodFunc() time.Duration {
	return 0
}

// New creates a FCD controller
func New() vTypes.Controller {
	return &controller{}
}

func (c *controller) Init(config *vcfg.Config) error {
	client, err := k8s.NewClient(config.Global.ServiceAccount)
	if err != nil {
		return fmt.Errorf("Creating Kubernetes client failed. Err: %v", err)
	}

	informMgr := k8s.NewInformer(&client)
	connMgr := cm.NewConnectionManager(config, informMgr.GetSecretListener())
	informMgr.Listen()

	c.client = &client
	c.cfg = config
	c.connMgr = connMgr
	c.informMgr = informMgr

	//VC check... FCD is only supported in 6.5+
	for vc := range connMgr.VsphereInstanceMap {
		api, err := connMgr.APIVersion(vc)
		if err != nil {
			klog.Errorf("APIVersion failed err=%v", err)
			return err
		}

		if err = checkAPI(api); err != nil {
			klog.Errorf("checkAPI failed err=%v", err)
			return err
		}
	}

	return nil
}

func (c *controller) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {

	// Get create params
	params := req.GetParameters()

	// Get accessibility
	accessibility := req.GetAccessibilityRequirements()

	// Volume Name
	volName := req.GetName()

	//check for required parameters
	if params == nil {
		msg := "Create parameters is a required parameter."
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	} else if len(volName) == 0 {
		msg := "Volume name is a required parameter."
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	} else if len(params[AttributeFirstClassDiskParentType]) == 0 {
		msg := fmt.Sprintf("Volume parameter %s is a required parameter.", AttributeFirstClassDiskParentType)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	} else if len(params[AttributeFirstClassDiskParentName]) == 0 {
		msg := fmt.Sprintf("Volume parameter %s is a required parameter.", AttributeFirstClassDiskParentName)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	// Volume Size - Default is 10 GiB
	volSizeBytes := int64(DefaultGbDiskSize * GbInBytes)
	if req.GetCapacityRange() != nil && req.GetCapacityRange().RequiredBytes != 0 {
		volSizeBytes = int64(req.GetCapacityRange().GetRequiredBytes())
	}
	volSizeMB := int64(volumeutil.RoundUpSize(volSizeBytes, GbInBytes)) * 1024

	// Volume Type
	datastoreType := vclib.TypeDatastoreCluster
	volType := params[AttributeFirstClassDiskParentType]
	if volType == string(vclib.TypeDatastore) {
		datastoreType = vclib.TypeDatastore
	}

	datastoreName := params[AttributeFirstClassDiskParentName]
	zone := params[AttributeFirstClassDiskZone]
	region := params[AttributeFirstClassDiskRegion]

	// Please see function for more details
	var err error
	var discoveryInfo *cm.ZoneDiscoveryInfo

	if accessibility != nil && (accessibility.GetRequisite() != nil || accessibility.GetPreferred() != nil) {
		log.Infoln("WhichVCandDCByZone with Topology Support")
		if accessibility.GetRequisite() != nil {
			log.Infoln("Requisite Topology Exists")
			requisites := accessibility.GetRequisite()
			for _, requisite := range requisites {
				segments := requisite.GetSegments()
				reqRegion := segments[LabelZoneRegion]
				reqZone := segments[LabelZoneFailureDomain]
				discoveryInfo, err = c.connMgr.WhichVCandDCByZone(ctx, c.cfg.Labels.Zone, c.cfg.Labels.Region, reqZone, reqRegion)
				if err == nil {
					log.Infof("WhichVCandDCByZone Succeeded in region=%s zone=%s", reqRegion, reqZone)
					break
				}
			}
		} else {
			log.Infoln("Using Perferred Topology")
			for _, preferred := range accessibility.GetPreferred() {
				segments := preferred.GetSegments()
				reqRegion := segments[LabelZoneRegion]
				reqZone := segments[LabelZoneFailureDomain]
				discoveryInfo, err = c.connMgr.WhichVCandDCByZone(ctx, c.cfg.Labels.Zone, c.cfg.Labels.Region, reqZone, reqRegion)
				if err == nil {
					log.Infof("WhichVCandDCByZone Succeeded in region=%s zone=%s", reqRegion, reqZone)
					break
				}
			}
		}
	} else {
		log.Infoln("WhichVCandDCByZone with Legacy region/zone")
		discoveryInfo, err = c.connMgr.WhichVCandDCByZone(ctx, c.cfg.Labels.Zone, c.cfg.Labels.Region, zone, region)
	}

	if err != nil {
		msg := fmt.Sprintf("Failed to retrieve VC/DC based on zone %s. Err: %v", zone, err)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	firstClassDisk, err := discoveryInfo.DataCenter.GetFirstClassDisk(
		ctx, datastoreName, datastoreType, volName, vclib.FindFCDByName)
	if err == nil {
		log.Warningf("Volume with name %s already exists. Checking for similar parameters.", volName)

		if firstClassDisk.Config.CapacityInMB != volSizeMB {
			msg := fmt.Sprintf("Volume already exists but requesting different size. Existing %d != Requested %d",
				firstClassDisk.Config.CapacityInMB, volSizeMB)
			log.Errorf(msg)
			return nil, status.Errorf(codes.AlreadyExists, msg)
		}
	} else {
		err = discoveryInfo.DataCenter.CreateFirstClassDisk(ctx, datastoreName, datastoreType, volName, volSizeMB)
		if err != nil {
			msg := fmt.Sprintf("CreateFirstClassDisk failed. Err: %v", err)
			log.Errorf(msg)
			return nil, status.Errorf(codes.Internal, msg)
		}

		firstClassDisk, err = discoveryInfo.DataCenter.GetFirstClassDisk(
			ctx, datastoreName, datastoreType, volName, vclib.FindFCDByName)
		if err != nil {
			msg := fmt.Sprintf("GetFirstClassDiskByName(%s) failed. Err: %v", volName, err)
			log.Errorf(msg)
			return nil, status.Errorf(codes.Internal, msg)
		}
	}

	attributes := make(map[string]string)
	attributes[AttributeFirstClassDiskType] = FirstClassDiskTypeString
	attributes[AttributeFirstClassDiskVcenter] = discoveryInfo.VcServer
	attributes[AttributeFirstClassDiskDatacenter] = discoveryInfo.DataCenter.Name()
	attributes[AttributeFirstClassDiskName] = firstClassDisk.Config.Name
	attributes[AttributeFirstClassDiskParentType] = string(firstClassDisk.ParentType)
	if firstClassDisk.ParentType == vclib.TypeDatastoreCluster {
		attributes[AttributeFirstClassDiskParentName] = firstClassDisk.StoragePodInfo.Summary.Name
		attributes[AttributeFirstClassDiskOwningDatastore] = firstClassDisk.DatastoreInfo.Info.Name
	} else {
		attributes[AttributeFirstClassDiskParentName] = firstClassDisk.DatastoreInfo.Info.Name
	}

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      firstClassDisk.Config.Id.Id,
			CapacityBytes: int64(units.FileSize(firstClassDisk.Config.CapacityInMB * MbInBytes)),
			VolumeContext: attributes,
			//TODO: ContentSource?
		},
	}

	return resp, nil
}

func (c *controller) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {

	//check for required parameters
	if len(req.VolumeId) == 0 {
		msg := "Volume ID is a required parameter."
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	discoveryInfo, err := c.connMgr.WhichVCandDCByFCDId(ctx, req.VolumeId)
	if err == vclib.ErrNoDiskIDFound {
		log.Warningf("Failed to retrieve VC/DC based on FCDID %s. Err: %v", req.VolumeId, err)
		return &csi.DeleteVolumeResponse{}, nil
	} else if err != nil {
		msg := fmt.Sprintf("WhichVCandDCByFCDId(%s) failed. Err: %v", req.VolumeId, err)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	// Volume Type
	var datastoreName string
	datastoreType := discoveryInfo.FCDInfo.ParentType
	if datastoreType == vclib.TypeDatastore {
		datastoreName = discoveryInfo.FCDInfo.DatastoreInfo.Info.Name
	} else {
		datastoreName = discoveryInfo.FCDInfo.StoragePodInfo.Summary.Name
	}

	err = discoveryInfo.DataCenter.DeleteFirstClassDisk(ctx, datastoreName, datastoreType, req.VolumeId)
	if err != nil {
		msg := fmt.Sprintf("DeleteFirstClassDisk(%s) failed. Err: %v", req.VolumeId, err)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (c *controller) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {

	//check for required parameters
	if len(req.VolumeId) == 0 {
		msg := "Volume ID is a required parameter."
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	} else if len(req.NodeId) == 0 {
		msg := "Node ID is a required parameter."
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	discoveryInfo, err := c.connMgr.WhichVCandDCByFCDId(ctx, req.VolumeId)
	if err != nil {
		msg := fmt.Sprintf("WhichVCandDCByFCDId(%s) failed. Err: %v", req.VolumeId, err)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	fcd := discoveryInfo.FCDInfo

	vm, err := discoveryInfo.DataCenter.GetVMByDNSName(ctx, req.NodeId)
	if err != nil {
		log.Errorf("GetVMByDNSName(%s) failed. Err: %v", req.NodeId, err)
		return nil, err
	}

	filePath := fcd.Config.Backing.(*types.BaseConfigInfoDiskFileBackingInfo).FilePath
	options := &vclib.VolumeOptions{SCSIControllerType: vclib.PVSCSIControllerType}
	diskUUID, err := vm.AttachDisk(ctx, filePath, options)
	if err != nil {
		log.Errorf("AttachDisk(%s = %s) failed. Err: %v", fcd.Config.Name, filePath, err)
		return nil, err
	}

	log.Infof("AttachDisk(%s) succeeded with UUID: %s", filePath, diskUUID)

	publishInfo := make(map[string]string, 0)
	publishInfo[AttributeFirstClassDiskType] = FirstClassDiskTypeString
	publishInfo[AttributeFirstClassDiskVcenter] = discoveryInfo.VcServer
	publishInfo[AttributeFirstClassDiskDatacenter] = discoveryInfo.DataCenter.Name()
	publishInfo[AttributeFirstClassDiskName] = fcd.Config.Name
	publishInfo[AttributeFirstClassDiskParentType] = string(fcd.ParentType)
	if fcd.ParentType == vclib.TypeDatastoreCluster {
		publishInfo[AttributeFirstClassDiskParentName] = fcd.StoragePodInfo.Summary.Name
		publishInfo[AttributeFirstClassDiskOwningDatastore] = fcd.DatastoreInfo.Info.Name
	} else {
		publishInfo[AttributeFirstClassDiskParentName] = fcd.DatastoreInfo.Info.Name
	}
	publishInfo[AttributeFirstClassDiskPage83Data] = diskUUID

	resp := &csi.ControllerPublishVolumeResponse{
		PublishContext: publishInfo,
	}

	return resp, nil
}

func (c *controller) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {

	//check for required parameters
	if len(req.VolumeId) == 0 {
		msg := "Volume ID is a required parameter."
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	} else if len(req.NodeId) == 0 {
		msg := "Node ID is a required parameter."
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	discoveryInfo, err := c.connMgr.WhichVCandDCByFCDId(ctx, req.VolumeId)
	if err != nil {
		msg := fmt.Sprintf("WhichVCandDCByFCDId(%s) failed. Err: %v", req.VolumeId, err)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	fcd := discoveryInfo.FCDInfo

	vm, err := discoveryInfo.DataCenter.GetVMByDNSName(ctx, req.NodeId)
	if err != nil {
		log.Errorf("GetVMByDNSName(%s) failed. Err: %v", req.NodeId, err)
		return nil, err
	}

	filePath := fcd.Config.Backing.(*types.BaseConfigInfoDiskFileBackingInfo).FilePath
	err = vm.DetachDisk(ctx, filePath)
	if err != nil {
		log.Errorf("DetachDisk(%s = %s) failed. Err: %v", fcd.Config.Name, filePath, err)
		return nil, err
	}

	resp := &csi.ControllerUnpublishVolumeResponse{}

	return resp, nil
}

func (c *controller) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {

	return &csi.ValidateVolumeCapabilitiesResponse{}, nil
}

func (c *controller) ListVolumes(
	ctx context.Context,
	req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {

	var err error
	firstClassDisks := getAllFCDs(ctx, c.connMgr)

	total := len(firstClassDisks)

	start := 0
	if req.StartingToken != "" {
		start, err = strconv.Atoi(req.StartingToken)
		if err != nil {
			msg := fmt.Sprintf("Invalid starting token %s. Err: %v", req.StartingToken, err)
			log.Errorf(msg)
			return nil, status.Errorf(codes.Internal, msg)
		}
	}

	stop := total
	if req.MaxEntries != 0 && stop > int(req.MaxEntries) {
		stop = start + int(req.MaxEntries) - 1
	}

	log.Infof("Start: %d, End: %d, Total: %d", start, stop, total)

	resp := &csi.ListVolumesResponse{}

	subsetFirstClassDisks := firstClassDisks
	if start > total {
		msg := fmt.Sprintf("Invalid start token %d. Greater than total items %d.", start, total)
		log.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	} else if stop >= total {
		subsetFirstClassDisks = firstClassDisks[start:]
	} else if stop < total {
		subsetFirstClassDisks = firstClassDisks[start:(stop + 1)]
	}

	for _, firstClassDisk := range subsetFirstClassDisks {
		attributes := make(map[string]string)
		attributes[AttributeFirstClassDiskType] = FirstClassDiskTypeString
		attributes[AttributeFirstClassDiskVcenter] = removePortFromHost(firstClassDisk.Datacenter.Client().URL().Host)
		attributes[AttributeFirstClassDiskDatacenter] = firstClassDisk.Datacenter.Name()
		attributes[AttributeFirstClassDiskName] = firstClassDisk.Config.Name
		attributes[AttributeFirstClassDiskParentType] = string(firstClassDisk.ParentType)
		if firstClassDisk.ParentType == vclib.TypeDatastoreCluster {
			attributes[AttributeFirstClassDiskParentName] = firstClassDisk.StoragePodInfo.Summary.Name
			attributes[AttributeFirstClassDiskOwningDatastore] = firstClassDisk.DatastoreInfo.Info.Name
		} else {
			attributes[AttributeFirstClassDiskParentName] = firstClassDisk.DatastoreInfo.Info.Name
		}

		resp.Entries = append(resp.Entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      firstClassDisk.Config.Id.Id,
				CapacityBytes: int64(units.FileSize(firstClassDisk.Config.CapacityInMB * MbInBytes)),
				VolumeContext: attributes,
				//TODO: ContentSource?
			},
		})
	}

	if stop < total {
		resp.NextToken = strconv.Itoa(stop + 1)
		log.Infoln("Next token is", resp.NextToken)
	}

	return resp, nil
}

func (c *controller) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {

	return nil, nil
}

func (c *controller) ControllerGetCapabilities(
	ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (c *controller) CreateSnapshot(
	ctx context.Context,
	req *csi.CreateSnapshotRequest) (
	*csi.CreateSnapshotResponse, error) {

	return nil, nil
}

func (c *controller) DeleteSnapshot(
	ctx context.Context,
	req *csi.DeleteSnapshotRequest) (
	*csi.DeleteSnapshotResponse, error) {

	return nil, nil
}

func (c *controller) ListSnapshots(
	ctx context.Context,
	req *csi.ListSnapshotsRequest) (
	*csi.ListSnapshotsResponse, error) {

	return nil, nil
}
