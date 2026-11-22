// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// thirdPartyCSIClient dials (and caches) a connection to a configured
// third-party CSI driver's own Unix socket -- kairon-node acting as a
// generic CSI client, the same "no Pod for kubelet to trigger this
// through" reasoning csiNodeClient already has for Kairon's own driver,
// just against an operator-named socket instead of a-fixed one. Returns a
// clear error for a driver name not present in
// Agent.ThirdPartyCSIDrivers -- fail-closed, the same posture an unlisted
// VFIO BDF already has against KAIRON_VFIO_ALLOWLIST.
func (a *Agent) thirdPartyCSIClient(driver string) (csi.NodeClient, error) {
	socket, ok := a.ThirdPartyCSIDrivers[driver]
	if !ok {
		return nil, fmt.Errorf("CSI driver %q is not in this node's third-party allowlist (-third-party-csi-drivers/$KAIRON_THIRD_PARTY_CSI_DRIVERS) -- refusing to dial an unauthorized driver socket; see docs/guides/machine-storage-thirdparty-csi.md", driver)
	}
	if conn, ok := a.thirdPartyCSIConns[driver]; ok {
		return csi.NewNodeClient(conn), nil
	}
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial third-party CSI driver %q at %s: %w", driver, socket, err)
	}
	if a.thirdPartyCSIConns == nil {
		a.thirdPartyCSIConns = map[string]*grpc.ClientConn{}
	}
	a.thirdPartyCSIConns[driver] = conn
	return csi.NewNodeClient(conn), nil
}

// resolveThirdPartyCSIVolume is resolveCSIVolume's counterpart for a
// third-party driver (see docs/guides/machine-storage-thirdparty-csi.md):
//
//  1. Drivers whose CSIDriver object says attachRequired get a
//     VolumeAttachment, exactly what kubelet's attach/detach controller
//     creates for a Pod; the driver's external-attacher runs
//     ControllerPublishVolume and its attachmentMetadata becomes the
//     PublishContext for NodeStage/NodePublish. Until it reports attached
//     this returns an error and the next reconcile tick retries.
//  2. nodeStageSecretRef / nodePublishSecretRef are resolved only from
//     Agent.ThirdPartyCSISecretNamespace; every key of the Secret is
//     passed, as kubelet does.
//  3. staging/publish paths are synthesized from the Machine's own
//     deterministic RuntimeName() rather than a kubelet-assigned,
//     Pod-UID-keyed path -- there is no Pod here for a Pod UID to come
//     from. Most CSI drivers treat these paths as opaque, but a driver
//     that specifically depends on kubelet's own path convention (some
//     drivers key internal state off it) may not behave correctly here;
//     a real, named compatibility risk, not a hidden one.
func (a *Agent) resolveThirdPartyCSIVolume(ctx context.Context, m model.Machine, pv model.PersistentVolume) (string, csiVolumeStatus, error) {
	src := pv.Spec.CSI
	client, err := a.thirdPartyCSIClient(src.Driver)
	if err != nil {
		return "", csiVolumeStatus{}, err
	}
	// A Block-mode volume is published as a device node at the target path
	// itself; a Filesystem one as a directory holding disk.img.
	block := pv.Spec.VolumeMode == "Block"
	bootPath := func(publish string) string {
		if block {
			return publish
		}
		return filepath.Join(publish, bootDiskFileName)
	}

	if m.Status.VolumeStagingPath != "" && m.Status.VolumePublishPath != "" && m.Status.VolumeHandle == src.VolumeHandle && m.Status.VolumeDriver == src.Driver {
		return bootPath(m.Status.VolumePublishPath), csiVolumeStatus{
			StagingPath: m.Status.VolumeStagingPath, PublishPath: m.Status.VolumePublishPath, VolumeID: m.Status.VolumeHandle, Driver: src.Driver,
		}, nil
	}

	publishContext, err := a.ensureCSIAttachment(ctx, pv)
	if err != nil {
		return "", csiVolumeStatus{}, err
	}
	stageSecrets, err := a.resolveThirdPartyCSISecrets(ctx, "nodeStageSecretRef", src.NodeStageSecretRef)
	if err != nil {
		return "", csiVolumeStatus{}, err
	}
	publishSecrets, err := a.resolveThirdPartyCSISecrets(ctx, "nodePublishSecretRef", src.NodePublishSecretRef)
	if err != nil {
		return "", csiVolumeStatus{}, err
	}

	staging := filepath.Join(a.CSIStagingDir, "thirdparty", src.Driver, m.RuntimeName())
	publish := filepath.Join(a.CSIPublishDir, "thirdparty", src.Driver, m.RuntimeName())
	volumeCapability := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: src.FSType}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	if block {
		volumeCapability.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
		// The CO owns the target's parent directory; the driver creates the
		// device file itself.
		if err := os.MkdirAll(filepath.Dir(publish), 0o750); err != nil {
			return "", csiVolumeStatus{}, fmt.Errorf("create CSI publish parent for PersistentVolume %s: %w", pv.Metadata.Name, err)
		}
	}

	if _, err := client.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          src.VolumeHandle,
		PublishContext:    publishContext,
		StagingTargetPath: staging,
		VolumeCapability:  volumeCapability,
		VolumeContext:     src.VolumeAttributes,
		Secrets:           stageSecrets,
	}); err != nil {
		return "", csiVolumeStatus{}, fmt.Errorf("NodeStageVolume for PersistentVolume %s (driver %s): %w", pv.Metadata.Name, src.Driver, err)
	}
	if _, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          src.VolumeHandle,
		PublishContext:    publishContext,
		StagingTargetPath: staging,
		TargetPath:        publish,
		VolumeCapability:  volumeCapability,
		VolumeContext:     src.VolumeAttributes,
		Readonly:          src.ReadOnly,
		Secrets:           publishSecrets,
	}); err != nil {
		return "", csiVolumeStatus{}, fmt.Errorf("NodePublishVolume for PersistentVolume %s (driver %s): %w", pv.Metadata.Name, src.Driver, err)
	}

	return bootPath(publish), csiVolumeStatus{StagingPath: staging, PublishPath: publish, VolumeID: src.VolumeHandle, Driver: src.Driver}, nil
}

// csiAttachmentName is kairon-node's VolumeAttachment name for a volume on
// this node. The "kairon-" prefix keeps it distinct from the "csi-" names
// kubelet's attach/detach controller owns, which would otherwise delete
// an attachment no Pod references.
func (a *Agent) csiAttachmentName(driver, volumeHandle string) string {
	return fmt.Sprintf("kairon-%x", sha256.Sum256([]byte(volumeHandle+driver+a.NodeName)))
}

// csiAttachRequired reports whether driver wants ControllerPublish. A
// missing CSIDriver object means "no", unlike Kubernetes' own default:
// without one there is nothing that says an external-attacher is running
// to act on a VolumeAttachment, and waiting forever would be worse.
func (a *Agent) csiAttachRequired(ctx context.Context, driver string) (bool, error) {
	if a.Kube == nil {
		return false, nil
	}
	d, err := a.Kube.GetCSIDriver(ctx, driver)
	if kube.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get CSIDriver %s: %w", driver, err)
	}
	return d.Spec.AttachRequired != nil && *d.Spec.AttachRequired, nil
}

// ensureCSIAttachment returns the PublishContext for pv on this node,
// creating its VolumeAttachment when the driver requires attach.
func (a *Agent) ensureCSIAttachment(ctx context.Context, pv model.PersistentVolume) (map[string]string, error) {
	src := pv.Spec.CSI
	required, err := a.csiAttachRequired(ctx, src.Driver)
	if err != nil || !required {
		return nil, err
	}
	if a.NodeName == "" {
		return nil, fmt.Errorf("CSI driver %s requires attach but this node has no name configured", src.Driver)
	}
	name := a.csiAttachmentName(src.Driver, src.VolumeHandle)
	va, err := a.Kube.GetVolumeAttachment(ctx, name)
	if kube.IsNotFound(err) {
		pvName := pv.Metadata.Name
		va, err = a.Kube.CreateVolumeAttachment(ctx, model.VolumeAttachment{
			Metadata: model.ObjectMeta{Name: name},
			Spec: model.VolumeAttachmentSpec{
				Attacher: src.Driver,
				NodeName: a.NodeName,
				Source:   model.VolumeAttachmentSource{PersistentVolumeName: &pvName},
			},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("VolumeAttachment %s for PersistentVolume %s: %w", name, pv.Metadata.Name, err)
	}
	if va.Spec.NodeName != a.NodeName || va.Spec.Attacher != src.Driver {
		return nil, fmt.Errorf("VolumeAttachment %s exists for node %q / attacher %q, not %q / %q", name, va.Spec.NodeName, va.Spec.Attacher, a.NodeName, src.Driver)
	}
	if va.Status.AttachError != nil && va.Status.AttachError.Message != "" {
		return nil, fmt.Errorf("VolumeAttachment %s: attach failed: %s", name, va.Status.AttachError.Message)
	}
	if !va.Status.Attached {
		return nil, fmt.Errorf("waiting for %s's external-attacher to attach PersistentVolume %s (VolumeAttachment %s)", src.Driver, pv.Metadata.Name, name)
	}
	return va.Status.AttachmentMetadata, nil
}

// releaseCSIAttachment deletes this node's VolumeAttachment for a volume,
// if any; the external-attacher then runs ControllerUnpublishVolume.
func (a *Agent) releaseCSIAttachment(ctx context.Context, driver, volumeHandle string) error {
	if a.Kube == nil || a.NodeName == "" {
		return nil
	}
	name := a.csiAttachmentName(driver, volumeHandle)
	if err := a.Kube.DeleteVolumeAttachment(ctx, name); err != nil && !kube.IsNotFound(err) {
		return fmt.Errorf("delete VolumeAttachment %s: %w", name, err)
	}
	return nil
}

func (a *Agent) resolveThirdPartyCSISecrets(ctx context.Context, field string, ref *model.SecretReference) (map[string]string, error) {
	if ref == nil {
		return nil, nil
	}
	if ref.Name == "" {
		return nil, fmt.Errorf("%s.name is required when %s is set", field, field)
	}
	if a.ThirdPartyCSISecretNamespace == "" {
		return nil, fmt.Errorf("PersistentVolume names %s %q but this node has no third-party CSI secret namespace configured (--third-party-csi-secret-namespace) -- see docs/guides/machine-storage-thirdparty-csi.md", field, ref.Name)
	}
	ns := ref.Namespace
	if ns == "" {
		ns = a.ThirdPartyCSISecretNamespace
	}
	if ns != a.ThirdPartyCSISecretNamespace {
		return nil, fmt.Errorf("%s namespace %q is outside this node's third-party CSI secret namespace %q", field, ns, a.ThirdPartyCSISecretNamespace)
	}
	if a.Kube == nil {
		return nil, fmt.Errorf("no kubernetes client configured -- cannot resolve %s %s/%s", field, ns, ref.Name)
	}
	secret, err := a.Kube.GetSecret(ctx, ns, ref.Name)
	if err != nil {
		return nil, fmt.Errorf("get %s Secret %s/%s: %w", field, ns, ref.Name, err)
	}
	out := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		out[k] = string(v)
	}
	return out, nil
}

// teardownThirdPartyCSIVolume is teardownCSIVolume's counterpart for a
// Machine whose status.volumeDriver names a third-party driver. The
// VolumeAttachment goes last, after the volume is unstaged on this node.
func (a *Agent) teardownThirdPartyCSIVolume(ctx context.Context, m model.Machine) error {
	client, err := a.thirdPartyCSIClient(m.Status.VolumeDriver)
	if err != nil {
		return err
	}
	if m.Status.VolumePublishPath != "" {
		if _, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
			VolumeId: m.Status.VolumeHandle, TargetPath: m.Status.VolumePublishPath,
		}); err != nil {
			return fmt.Errorf("NodeUnpublishVolume (driver %s): %w", m.Status.VolumeDriver, err)
		}
	}
	if m.Status.VolumeStagingPath != "" {
		if _, err := client.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
			VolumeId: m.Status.VolumeHandle, StagingTargetPath: m.Status.VolumeStagingPath,
		}); err != nil {
			return fmt.Errorf("NodeUnstageVolume (driver %s): %w", m.Status.VolumeDriver, err)
		}
	}
	return a.releaseCSIAttachment(ctx, m.Status.VolumeDriver, m.Status.VolumeHandle)
}
