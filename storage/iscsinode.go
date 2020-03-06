package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	iscsi_lib "github.com/kubernetes-csi/csi-lib-iscsi/iscsi"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume/util"
)

var trackingFilePath = "/var/lib/infinidat/tracking/"

type iscsiDisk struct {
	Portals         []string
	Iqn             string
	lun             int32
	Iface           string
	chapDiscovery   bool
	doDiscovery     bool
	chapSession     bool
	secret          map[string]string
	sessionSecret   iscsi_lib.Secrets
	discoverySecret iscsi_lib.Secrets
	VolName         string
	isBlock         bool
	ssdEnabled      string
}

type iscsiDiskMounter struct {
	*iscsiDisk
	readOnly     bool
	fsType       string
	mountOptions []string
	mounter      *mount.SafeFormatAndMount
	exec         mount.Exec
	deviceUtil   util.DeviceUtil
	targetPath   string
	connector    *iscsi_lib.Connector
}

type iscsiDiskUnmounter struct {
	*iscsiDisk
	mounter mount.Interface
	exec    mount.Exec
}

type volumePublishInfo struct {
	VolumeName  string   `json:"volume_name,omitempty"`
	Lun         int      `json:"lun,omitempty"`
	DoDiscovery bool     `json:"do_discovery,omitempty"`
	Targets     []target `json:"targets,omitempty"`
}

type target struct {
	Iqn    string `json:"iqn,omitempty"`
	Portal string `json:"portal,omitempty"`
	Port   string `json:"port,omitempty"`
}

func (iscsi *iscsistorage) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest) (
	*csi.NodePublishVolumeResponse, error) {
	log.Debugf("Called NodePublishVolume with volume ID %s", req.GetVolumeId())
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	iscsiInfo, err := iscsi.getISCSIInfo(req)
	if err != nil {
		return &csi.NodePublishVolumeResponse{}, status.Error(codes.Internal, err.Error())
	}

	switch volCap.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		iscsiInfo.isBlock = true
	}

	diskMounter := iscsi.getISCSIDiskMounter(iscsiInfo, req)
	_, err = iscsi.AttachDisk(*diskMounter)
	if err != nil {
		log.Errorf("Failed to mount volume with error %v", err)
		return &csi.NodePublishVolumeResponse{}, status.Error(codes.Internal, err.Error())
	}
	log.Debug("Node Published Successfully")
	return &csi.NodePublishVolumeResponse{}, nil
}

func (iscsi *iscsistorage) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest) (
	csiResp *csi.NodeUnpublishVolumeResponse, err error) {
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from ISCSI NodeUnpublishVolume  " + fmt.Sprint(res))
		}
	}()
	log.Debugf("Called Node UnPublish for target %s", req.TargetPath)
	diskUnmounter := iscsi.getISCSIDiskUnmounter(req)
	targetPath := req.GetTargetPath()
	err = iscsi.DetachDisk(*diskUnmounter, targetPath)
	if err != nil {
		return &csi.NodeUnpublishVolumeResponse{}, status.Error(codes.Internal, err.Error())
	}
	log.Debug("Node UnPublished Successfully")
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (iscsi *iscsistorage) AttachDisk(d iscsiDiskMounter) (mntPath string, err error) {
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from ISCSI AttachDisk  " + fmt.Sprint(res))
		}
	}()
	log.Info("Called AttachDisk")
	log.WithFields(log.Fields{"iqn": d.iscsiDisk.Iqn, "lun": d.iscsiDisk.lun,
		"DoCHAPDiscovery": d.connector.DoCHAPDiscovery}).Info("Mounting Volume")

	if "debug" == log.GetLevel().String() {
		//iscsi_lib.EnableDebugLogging(log.New().Writer())
	}

	devicePath, err := iscsi_lib.Connect(*d.connector)
	if err != nil {
		log.Errorf("Disk Connect failed with error %v", err)
		return "", err
	}
	if devicePath == "" {
		return "", fmt.Errorf("device path not recieved")
	}

	// Mount device
	mntPath = d.targetPath
	err = d.mounter.MakeDir(filepath.Dir(trackingFilePath))
	if err != nil {
		log.Errorf("failed to create target directory %q: %v", filepath.Dir(d.targetPath), err)
		//return "", fmt.Errorf("failed to create target directory for raw block bind mount: %v", err)
	}
	trackingFile := trackingFilePath + d.VolName + ".json"
	err = writeStagedTargetPath(trackingFile, mntPath)
	log.Debug("error while writing writeStagedTargetPath : ", err)
	if err != nil {
		log.Debug("Failed to add stagingTargetPath")
	}
	notMnt, err := d.mounter.IsLikelyNotMountPoint(mntPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("Could not validate mount point, error :%v", err)
	}
	if !notMnt {
		log.Infof("volume already mounted at : %s", mntPath)
		return "", nil
	}
	var options []string
	if d.isBlock {
		log.Debugf("Block volume will be mount at file %s", d.targetPath)
		if d.readOnly {
			return "", status.Error(codes.Internal, "Read only is not supported for Block Volume")
		}

		err = d.mounter.MakeDir(filepath.Dir(d.targetPath))
		if err != nil {
			log.Errorf("failed to create target directory %q: %v", filepath.Dir(d.targetPath), err)
			return "", fmt.Errorf("failed to create target directory for raw block bind mount: %v", err)
		}

		err = d.mounter.MakeFile(d.targetPath)
		if err != nil {
			log.Errorf("failed to create target file %q: %v", d.targetPath, err)
			return "", fmt.Errorf("failed to create target file for raw block bind mount: %v", err)
		}

		symLink, err := filepath.EvalSymlinks(devicePath)
		if err != nil {
			log.Errorf("could not resolve symlink %q: %v", devicePath, err)
			return "", fmt.Errorf("could not resolve symlink %q: %v", devicePath, err)
		}

		if !strings.HasPrefix(symLink, "/dev") {
			log.Errorf("resolved symlink %q for %q was unexpected", symLink, devicePath)
			return "", fmt.Errorf("resolved symlink %q for %q was unexpected", symLink, devicePath)
		}

		options := []string{"bind"}
		options = append(options, "rw")
		if err := d.mounter.Mount(symLink, d.targetPath, "", options); err != nil {
			log.Errorf("iscsi: failed to mount iscsi volume %s [%s] to %s, error %v", symLink, d.fsType, d.targetPath, err)
			return "", err
		}
		log.Debug("Block volume mounted successfully")
		return symLink, err
	} else {
		log.Debugf("Volume will be mounted at %s", d.targetPath)
		if err := os.MkdirAll(mntPath, 0750); err != nil {
			log.Errorf("iscsi: failed to mkdir %s, error", mntPath)
			return "", err
		}
		if d.readOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}
		options = append(options, d.mountOptions...)
		log.Debug("Trying to format and mount volume")
		err = d.mounter.FormatAndMount(devicePath, mntPath, d.fsType, options)
		if err != nil {
			log.Errorf("iscsi: failed to mount iscsi volume %s [%s] to %s, error %v", devicePath, d.fsType, mntPath, err)
			return "", err
		}
		log.Debug("Volume mounted successfully")

		file := path.Join(mntPath, d.VolName+".json")
		d.connector.SessionSecrets = iscsi_lib.Secrets{}
		d.connector.DiscoverySecrets = iscsi_lib.Secrets{}
		err = iscsi_lib.PersistConnector(d.connector, file)
		if err != nil {
			log.Errorf("failed to persist connection info: %v", err)
			return "", fmt.Errorf("unable to create persistence file for connection")
		}
		return devicePath, err
	}
	return "", errors.New("Unable to mount volume")
}

func (iscsi *iscsistorage) DetachDisk(c iscsiDiskUnmounter, targetPath string) (err error) {
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from ISCSI DetachDisk  " + fmt.Sprint(res))
		}
	}()
	log.Debug("Called DetachDisk")
	notMnt, err := c.mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Warnf("mount point '%s' already doesn't exist: '%s', return OK", targetPath, err)
			return nil
		}
		return err
	}
	if notMnt {
		if err := os.Remove(targetPath); err != nil {
			log.Errorf("Remove target path error: %s", err.Error())
		}
		return nil
	}

	if err := c.mounter.Unmount(targetPath); err != nil {
		return err
	}

	//load iscsi disk config from json file
	file := path.Join(targetPath, c.iscsiDisk.VolName+".json")
	if _, err := os.Stat(file); err == nil {
		connector, err := iscsi_lib.GetConnectorFromFile(file)
		if err != nil {
			log.Errorf("iscsi detach disk: failed to get iscsi config from path %s Error: %v", targetPath, err)
			return err
		}

		iqn := ""
		portals := []string{}
		if len(connector.Targets) > 0 {
			iqn = connector.Targets[0].Iqn
			for _, t := range connector.Targets {
				portals = append(portals, t.Portal)
			}
		}
		iscsi_lib.Disconnect(iqn, portals)
	}

	if err := os.RemoveAll(targetPath); err != nil {
		log.Errorf("iscsi: failed to remove mount path Error: %v", err)
		return err
	}
	log.Debug("DetachDisk Volume Successfully")
	return nil
}

func (iscsi *iscsistorage) getISCSIInfo(req *csi.NodePublishVolumeRequest) (*iscsiDisk, error) {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from ISCSI getISCSIInfo  " + fmt.Sprint(res))
		}
	}()
	volproto := strings.Split(req.GetVolumeId(), "$$")
	log.Debug("Called getISCSIInfo")
	volName := volproto[0]
	iface := req.GetVolumeContext()["iscsiInterface"]
	iqn := req.GetVolumeContext()["iqn"]
	portals := req.GetVolumeContext()["portals"]
	lun := req.GetPublishContext()["lun"]
	portalsList := strings.Split(portals, ",")
	secret := req.GetSecrets()
	sessionSecret, err := parseSessionSecret(secret)
	if err != nil {
		return nil, err
	}
	discoverySecret, _ := parseDiscoverySecret(secret)

	var bkportal []string
	for _, p := range portalsList {
		bkportal = append(bkportal, iscsi.portalMounter(p))
	}

	doDiscovery := true
	chapDiscovery := false
	if req.GetVolumeContext()["chapAuthentication"] == "true" {
		chapDiscovery = true
	}

	chapSession := false
	if req.GetVolumeContext()["chapAuthSession"] == "true" {
		chapSession = true
	}

	var lunVal int32
	if lun != "" {
		l, err := strconv.Atoi(lun)
		if err != nil {
			return nil, err
		}
		lunVal = int32(l)
	}
	log.Debug("getISCSIInfo : Parameter Configuration Complete")
	return &iscsiDisk{
		VolName:         volName,
		Portals:         bkportal,
		Iqn:             iqn,
		lun:             lunVal,
		Iface:           iface,
		chapDiscovery:   chapDiscovery,
		chapSession:     chapSession,
		secret:          secret,
		sessionSecret:   sessionSecret,
		discoverySecret: discoverySecret,
		doDiscovery:     doDiscovery}, nil
}

func buildISCSIConnector(iscsiInfo *iscsiDisk) *iscsi_lib.Connector {
	log.Debug("Called buildISCSIConnector")
	targets := []iscsi_lib.TargetInfo{}
	target := iscsi_lib.TargetInfo{}
	for _, p := range iscsiInfo.Portals {
		if p != "" {
			target.Iqn = iscsiInfo.Iqn
			if strings.Contains(p, ":") {
				arr := strings.Split(p, ":")
				target.Portal = arr[0]
				target.Port = arr[1]
			} else {
				target.Portal = p
				target.Port = "3260"
			}
			targets = append(targets, target)
		}
	}
	c := iscsi_lib.Connector{
		VolumeName:       iscsiInfo.VolName,
		Targets:          targets,
		Multipath:        len(iscsiInfo.Portals) > 1,
		DoDiscovery:      iscsiInfo.doDiscovery,
		DoCHAPDiscovery:  iscsiInfo.chapDiscovery,
		DiscoverySecrets: iscsiInfo.discoverySecret,
		Lun:              iscsiInfo.lun,
		Interface:        iscsiInfo.Iface,
		CheckInterval:    1,
		RetryCount:       10,
	}

	if iscsiInfo.sessionSecret != (iscsi_lib.Secrets{}) {
		c.SessionSecrets = iscsiInfo.sessionSecret
		if iscsiInfo.discoverySecret != (iscsi_lib.Secrets{}) {
			c.DiscoverySecrets = iscsiInfo.discoverySecret
		}
	}
	log.Debug("buildISCSIConnector: Connector configuration complete")
	return &c
}

func (iscsi *iscsistorage) getISCSIDiskMounter(iscsiInfo *iscsiDisk, req *csi.NodePublishVolumeRequest) *iscsiDiskMounter {
	log.Debug("Called getISCSIDiskMounter")
	readOnly := req.GetReadonly()
	fsType := req.GetVolumeContext()["fstype"]
	mountOptions := req.GetVolumeCapability().GetMount().GetMountFlags()
	log.Debug("getISCSIDiskMounter: Parameter configuration complete")
	return &iscsiDiskMounter{
		iscsiDisk:    iscsiInfo,
		fsType:       fsType,
		readOnly:     readOnly,
		mountOptions: mountOptions,
		mounter:      &mount.SafeFormatAndMount{Interface: mount.New(""), Exec: mount.NewOsExec()},
		exec:         mount.NewOsExec(),
		targetPath:   req.GetTargetPath(),
		deviceUtil:   util.NewDeviceHandler(util.NewIOHandler()),
		connector:    buildISCSIConnector(iscsiInfo),
	}
}

func (iscsi *iscsistorage) getISCSIDiskUnmounter(req *csi.NodeUnpublishVolumeRequest) *iscsiDiskUnmounter {
	log.Debug("Called getISCSIDiskUnmounter")
	volproto := strings.Split(req.GetVolumeId(), "$$")
	log.Debug("getISCSIDiskUnmounter: Unmounter configuration completed ")
	return &iscsiDiskUnmounter{
		iscsiDisk: &iscsiDisk{
			VolName: volproto[0],
		},
		mounter: mount.New(""),
		exec:    mount.NewOsExec(),
	}
}

func (iscsi *iscsistorage) portalMounter(portal string) string {
	if !strings.Contains(portal, ":") {
		portal = portal + ":3260"
	}
	return portal
}

func parseSecret(secretParams string) map[string]string {
	var secret map[string]string
	if err := json.Unmarshal([]byte(secretParams), &secret); err != nil {
		return nil
	}
	return secret
}

func parseSessionSecret(secretParams map[string]string) (iscsi_lib.Secrets, error) {
	var ok bool
	secret := iscsi_lib.Secrets{}

	if len(secretParams) == 0 {
		return secret, nil
	}

	if secret.UserName, ok = secretParams["node.session.auth.username"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.session.auth.username not found in secret")
	}
	if secret.Password, ok = secretParams["node.session.auth.password"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.session.auth.password not found in secret")
	}
	if secret.UserNameIn, ok = secretParams["node.session.auth.username_in"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.session.auth.username_in not found in secret")
	}
	if secret.PasswordIn, ok = secretParams["node.session.auth.password_in"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.session.auth.password_in not found in secret")
	}

	secret.SecretsType = "chap"
	return secret, nil
}

func parseDiscoverySecret(secretParams map[string]string) (iscsi_lib.Secrets, error) {
	var ok bool
	secret := iscsi_lib.Secrets{}

	if len(secretParams) == 0 {
		return secret, nil
	}

	if secret.UserName, ok = secretParams["node.sendtargets.auth.username"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.sendtargets.auth.username not found in secret")
	}
	if secret.Password, ok = secretParams["node.sendtargets.auth.password"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.sendtargets.auth.password not found in secret")
	}
	if secret.UserNameIn, ok = secretParams["node.sendtargets.auth.username_in"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.sendtargets.auth.username_in not found in secret")
	}
	if secret.PasswordIn, ok = secretParams["node.sendtargets.auth.password_in"]; !ok {
		return iscsi_lib.Secrets{}, fmt.Errorf("node.sendtargets.auth.password_in not found in secret")
	}
	secret.SecretsType = "chap"
	return secret, nil
}

func (iscsi *iscsistorage) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	log.Info("NodeStageVolume called with ", req.GetPublishContext())
	hostID := req.GetPublishContext()["hostID"]
	hstID, _ := strconv.Atoi(hostID)
	log.Infof("publishig volume to host id is %s", hostID)
	//validate host exists
	if hstID < 1 {
		log.Error("hostID %d is not valid host ID")
		return &csi.NodeStageVolumeResponse{}, status.Error(codes.Internal, "not a valid host")
	}
	initiatorName := getInitiatorName()
	if initiatorName == "" {
		log.Errorf("initiator name not found")
		return &csi.NodeStageVolumeResponse{}, status.Error(codes.Internal, "Inititator name not found")
	}
	log.Info("try to create port for host")
	err := iscsi.cs.AddPortForHost(hstID, "ISCSI", initiatorName)
	if err != nil {
		log.Errorf("error creating host port ", err)
		return &csi.NodeStageVolumeResponse{}, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeStageVolumeResponse{}, nil
}
func (iscsi *iscsistorage) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func getInitiatorName() string {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from ISCSI getInitiatorName " + fmt.Sprint(res))
		}
	}()
	cmd := "cat /etc/iscsi/initiatorname.iscsi | grep InitiatorName="
	out, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		fmt.Sprintf("Failed to execute command: %s", cmd)
	}
	initiatorName := string(out)
	initiatorName = strings.TrimSuffix(initiatorName, "\n")
	log.Info("host initiator name %s ", initiatorName)
	arr := strings.Split(initiatorName, "=")
	return arr[1]
}

func (iscsi *iscsistorage) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest) (
	*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_UNKNOWN,
					},
				},
			},
		},
	}, nil
}

func (iscsi *iscsistorage) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest) (
	*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{}, nil
}

func (iscsi *iscsistorage) NodeGetVolumeStats(
	ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return &csi.NodeGetVolumeStatsResponse{}, status.Error(codes.Unimplemented, time.Now().String())

}

func (iscsi *iscsistorage) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from ISCSI AttachDisk  " + fmt.Sprint(res))
		}
	}()
	log.Debug("It is new n=build----------------------------------------")
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "no volume ID provided")
	}

	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "no volume path provided")
	}

	capacity := int64(req.GetCapacityRange().GetRequiredBytes())
	if capacity < gib {
		capacity = gib
		log.Warn("Volume Minimum capacity should be greater 1 GB")
	}

	//Check volume exist or not
	log.Debug("staging Target path : ", req.GetStagingTargetPath())
	log.Debug("Target path : ", req.GetVolumePath())
	if _, err := os.Stat(volumePath); os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "unable to find volume publish info needed for resize")
	}
	log.WithFields(log.Fields{
		"volumeId":      volumeID,
		"volumePath":    volumePath,
		"requiredBytes": capacity,
		"filepath":      volumePath,
	}).Debug("NodeExpandVolumeRequest values")
	//Trident Solution
	filePath := volumePath + volumeID + ".json"
	publishInfo, err := readStagedDeviceInfo(filePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}
	log.Debug("publishInfo : ", publishInfo)
	lunID := int(publishInfo.Lun)

	for _, target := range publishInfo.Targets {
		log.Debugf("Rescan targetIqn: %s, portal: %s\n", target.Iqn, target.Portal)
		baseArgs := []string{"-m", "node", "-T", target.Iqn, "-p", target.Portal}
		// Rescan sessions to discover newly mapped LUNs. Do not specify the interface when rescanning
		// to avoid establishing additional sessions to the same target.
		if _, err := execCommand("iscsiadm", append(baseArgs, []string{"-R"}...)...); err != nil {
			log.Debugf("Failed to rescan session, err: %v", err)
		}
	}

	log.WithFields(log.Fields{
		"lunID": lunID,
	}).Debug("PublishInfo for device to expand.")
	//Expand volume size
	size, err := expandFilesystem("resize2fs", filePath, volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	//ceph solution
	// volumePath = "/var/lib/kubelet/pods/9591c330-fb5c-44dd-8503-0fa89d4de160/volumes/kubernetes.io~csi/csi-0cadf25803/mount"
	// log.Debug("Volume path : ", volumePath)
	// notMnt, err := mount.PathExists(volumePath)
	// log.Debug("-----------------------------Mount path : ", notMnt)
	// log.Debug("-----------------------------Mount path  err : ", err)
	// if err != nil {
	// 	log.Debug("err after mount.PathExists : ", err)
	// 	if os.IsNotExist(err) {
	// 		return nil, status.Error(codes.NotFound, err.Error())
	// 	}
	// 	return nil, status.Error(codes.Internal, err.Error())
	// }
	// if !notMnt {
	// 	return &csi.NodeExpandVolumeResponse{}, nil
	// }

	// devicePath, err := getDevicePath(ctx, volumePath)
	// if err != nil {
	// 	return nil, status.Error(codes.Internal, err.Error())
	// }
	//var ns iscsiDiskMounter
	//diskMounter := &mount.SafeFormatAndMount{Interface: ns.mountInterface, Exec: ns.exec}
	// log.Debug("Able to create diskMounter object : ", diskMounter)
	// // TODO check size and return success or errorgo build
	// mountPoint := volumePath
	// volumePath += "/18425.json"
	//resizer := resizefs.NewResizeFs(diskMounter)
	// log.Debug("Able to create resizer object : ", resizer)
	// log.Debug("Noe volume path : ", volumePath)
	// log.Debug("mountPoint : ", mountPoint)
	// log.Debug("volumePath : ", volumePath)
	//ok, err := resizer.Resize(volumePath, mountPoint)
	// log.Debug("-------------ok : ", ok)
	// log.Debug("-------------err : ", err)
	// if ok {
	// 	log.Debug("Filesystem expanded by size")
	// }
	log.Debug("Filesystem expanded by size ", size)
	return &csi.NodeExpandVolumeResponse{}, err
}

func readStagedDeviceInfo(stagingTargetPath string) (*volumePublishInfo, error) {

	var publishInfo volumePublishInfo

	publishInfoBytes, err := ioutil.ReadFile(stagingTargetPath)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(publishInfoBytes, &publishInfo)
	if err != nil {
		return nil, err
	}

	log.Debug("Publish Info found")
	return &publishInfo, nil
}

func execCommand(name string, args ...string) ([]byte, error) {

	log.WithFields(log.Fields{
		"command": name,
		"args":    args,
	}).Debug("Execute command ")

	out, err := exec.Command(name, args...).CombinedOutput()

	log.WithFields(log.Fields{
		"command": name,
		"output":  string(out),
		"error":   err,
	}).Debug("Executed command ")

	return out, err
}

func getFilesystemSize(path string) (int64, error) {
	var buf syscall.Statfs_t
	err := syscall.Statfs(path, &buf)
	if err != nil {
		log.WithField("path", path).Errorf("Failed to statfs: %s", err)
		return 0, fmt.Errorf("couldn't get filesystem stats %s: %s", path, err)
	}

	size := int64(buf.Blocks) * buf.Bsize
	log.WithFields(log.Fields{
		"path":  path,
		"size":  size,
		"avail": buf.Bavail,
		"free":  buf.Bfree,
	}).Debug("Filesystem size information")
	return size, nil
}

func expandFilesystem(cmd string, cmdArguments string, tmpMountPoint string) (int64, error) {
	preExpandSize, err := getFilesystemSize(tmpMountPoint)
	if err != nil {
		return 0, err
	}

	// _, err = execCommand("bash", "-c")
	// if err != nil {
	// 	log.Errorf("pvresize filesystem failed; %s", err)
	// 	return 0, err
	// }
	//iscsiMounter := iscsiDiskMounter{mounter: &mount.SafeFormatAndMount{Interface: mount.New(""), Exec: mount.NewOsExec()}}
	// _, err = execCommand("bash", "-c", "pvresize", "/dev/sdbd")
	// if err != nil {
	// 	log.Errorf("pvresize filesystem failed; %s", err)
	// 	return 0, err
	// }
	// //lvextend -l +100%PVS LV_Name /dev/mapper/mpath1
	// _, err = execCommand("bash", "-c", "lvextend", "-l", "+100%PVS", cmdArguments, "/dev/sdbd")
	// if err != nil {
	// 	log.Errorf("pvresize filesystem failed; %s", err)
	// 	return 0, err
	// }

	_, err = execCommand(cmd, cmdArguments)
	if err != nil {
		log.Errorf("Expanding filesystem failed; %s", err)
		return 0, err
	}

	postExpandSize, err := getFilesystemSize(tmpMountPoint)
	if err != nil {
		return 0, err
	}

	if postExpandSize == preExpandSize {
		log.Warnf("Failed to expand filesystem; size=%d", postExpandSize)
	}

	return postExpandSize, nil
}

func writeStagedTargetPath(filepath string, stagingTargetPath string) error {
	data := map[string]string{"targetPath": stagingTargetPath}
	log.Debug("Data dictionary : ", data)
	volumeTrackingPublishInfoBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath, volumeTrackingPublishInfoBytes, 0600); err != nil {
		log.WithFields(log.Fields{
			"TargetPath": filepath,
			"error":      err.Error(),
		}).Error("Unable to write tracking file.")
		return err
	}

	return nil
}
