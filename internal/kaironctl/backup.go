// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const backupUsage = `usage:
  kaironctl backup create MACHINE [--name NAME] [--quiesce auto|required|never] [--atlas] [--atlas-bucket ID] [--atlas-volume NAME]... [--keep N] [--max-age-seconds N]
  kaironctl backup create MACHINE --s3-endpoint URL --s3-bucket NAME --s3-credentials SECRET [--s3-prefix P] [--s3-region R] [--s3-virtual-host]
      [--mode full|incremental|synthetic-full] [--parent BACKUP] [--encryption-key SECRET] [--immutable-for 30d]
  kaironctl backup create MACHINE --mode incremental|synthetic-full --parent BACKUP   (S3 settings from the parent)
  kaironctl backup create MACHINE --policy POLICY [--mode ...]                       (S3 settings from a MachineBackupPolicy)
  kaironctl backup chain MACHINE
  kaironctl backup list
  kaironctl backup restore BACKUP [--machine NAME] [--storage-class NAME] [--name NAME]
  kaironctl backup delete NAME`

func cmdBackup(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("%s", backupUsage))
	}
	fs := flag.NewFlagSet("backup "+args[0], flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	switch args[0] {
	case "create":
		if len(args) < 2 {
			fatal(fmt.Errorf("%s", backupUsage))
		}
		name := fs.String("name", "", "MachineBackup name (default MACHINE-<utc>)")
		quiesce := fs.String("quiesce", "", "guest filesystem freeze: auto (default), required or never")
		useAtlas := fs.Bool("atlas", false, "also back up the Machine's Atlas volumes to S3")
		bucket := fs.String("atlas-bucket", "", "Atlas bucket id (implies --atlas; default: the controller's --atlas-backup-bucket)")
		var volumes stringSliceFlag
		fs.Var(&volumes, "atlas-volume", "back up only this Atlas volume (repeatable; implies --atlas)")
		keep := fs.Int64("keep", 0, "Atlas keeps at most this many backups per volume")
		maxAge := fs.Int64("max-age-seconds", 0, "Atlas prunes backups of the volume older than this")
		var s3 s3Flags
		s3.register(fs)
		mode := fs.String("mode", "", "S3 backups: full (default), incremental or synthetic-full")
		parent := fs.String("parent", "", "S3 backups: the MachineBackup an incremental builds on, or the chain point a synthetic full consolidates")
		policy := fs.String("policy", "", "S3 backups: take the target from this MachineBackupPolicy (an incremental without --parent builds on its newest backup)")
		_ = fs.Parse(args[2:])
		var atlasSpec *model.MachineBackupAtlas
		if *useAtlas || *bucket != "" || len(volumes) > 0 || *keep > 0 || *maxAge > 0 {
			atlasSpec = &model.MachineBackupAtlas{BucketID: *bucket, VolumeNames: volumes, Keep: *keep, MaxAgeSeconds: *maxAge}
		}
		b := newBackup(*ns, args[1], *name, *quiesce, atlasSpec)
		if s3.set() {
			b.Spec.Target, b.Spec.Encryption, b.Spec.ImmutableFor = s3.target(), s3.encryption(), s3.immutableFor
		}
		b.Spec.Mode, b.Spec.Parent = *mode, *parent
		if err := inheritCBTTarget(ctx, kc, &b, *policy); err != nil {
			fatal(err)
		}
		out, err := submitBackup(ctx, kc, b)
		if err != nil {
			fatal(err)
		}
		okf("machinebackup/%s created", out.Metadata.Name)
	case "chain":
		if len(args) < 2 {
			fatal(fmt.Errorf("%s", backupUsage))
		}
		_ = fs.Parse(args[2:])
		items, err := kc.ListMachineBackupsNamespace(ctx, *ns)
		if err != nil {
			fatal(err)
		}
		printBackupChains(os.Stdout, model.BackupChains(items, args[1]))
	case "list":
		_ = fs.Parse(args[1:])
		items, err := kc.ListMachineBackupsNamespace(ctx, *ns)
		if err != nil {
			fatal(err)
		}
		printBackups(items)
	case "restore":
		if len(args) < 2 {
			fatal(fmt.Errorf("%s", backupUsage))
		}
		name := fs.String("name", "", "MachineBackupRestore name (default BACKUP-restore-<utc>)")
		machine := fs.String("machine", "", "restore the disks into this Machine (default: the backed-up Machine); it must be halted")
		storageClass := fs.String("storage-class", "", "StorageClass for restored Atlas volumes")
		_ = fs.Parse(args[2:])
		out, err := createBackupRestore(ctx, kc, *ns, args[1], *name, *machine, *storageClass)
		if err != nil {
			fatal(err)
		}
		okf("machinebackuprestore/%s created", out.Metadata.Name)
		fmt.Println("the disk half waits until the Machine is halted: kaironctl halt MACHINE")
	case "delete":
		if len(args) < 2 {
			fatal(fmt.Errorf("%s", backupUsage))
		}
		_ = fs.Parse(args[2:])
		if err := kc.DeleteMachineBackup(ctx, *ns, args[1]); err != nil {
			fatal(err)
		}
		okf("machinebackup/%s deleted", args[1])
	default:
		fatal(fmt.Errorf("unknown backup action %q\n%s", args[0], backupUsage))
	}
}

func printBackups(items []model.MachineBackup) {
	fmt.Printf("NAME\tMACHINE\tNODE\tPHASE\tKIND\tSIZE\tCHANGED\tQUIESCED\tMESSAGE\n")
	for _, b := range items {
		kind, size, changed, quiesced := "local", "-", "-", "-"
		if b.IsCBT() {
			kind = b.BackupMode()
		}
		if b.Status.Disk != nil && b.Status.Disk.Phase == model.BackupSucceeded {
			size = fmt.Sprintf("%dMiB", b.Status.Disk.SizeBytes>>20)
			quiesced = fmt.Sprint(b.Status.Disk.Quiesced)
			if b.IsCBT() {
				changed = fmt.Sprintf("%dMiB", b.Status.Disk.ChangedBytes>>20)
			}
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.Metadata.Name, b.Spec.MachineName, dash(b.Status.NodeName), style.Phase(os.Stdout, b.Status.Phase), kind, size, changed, quiesced, dash(b.Status.Message))
	}
}

// printBackupChains draws each chain as a tree: incrementals indent under
// their parent; "=" marks a synthetic full standing in for a point.
func printBackupChains(w io.Writer, chains []model.BackupChainView) {
	if len(chains) == 0 {
		_, _ = fmt.Fprintln(w, "no S3 backups")
		return
	}
	for i, c := range chains {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		_, _ = fmt.Fprintf(w, "chain %s (%d backups, %s stored)\n", c.ID, len(c.Points), mib(c.StoredBytes))
		for _, p := range c.Points {
			mark := "+"
			switch p.Kind {
			case model.BackupModeFull:
				mark = "*"
			case model.BackupModeSyntheticFull:
				mark = "="
			}
			at := "-"
			if p.PointInTime != nil {
				at = p.PointInTime.UTC().Format(time.RFC3339)
			}
			extra := ""
			if p.RetentionUntil != nil && time.Now().Before(*p.RetentionUntil) {
				extra += " locked-until=" + p.RetentionUntil.UTC().Format("2006-01-02")
			}
			if p.Deleting {
				extra += " deleting"
			}
			_, _ = fmt.Fprintf(w, "%s%s %s  #%d %s %s  %s  stored=%s changed=%s%s\n", strings.Repeat("  ", p.Depth), mark, p.Name, p.Sequence, p.Kind, dash(p.Phase), at, mib(p.StoredBytes), mib(p.ChangedBytes), extra)
		}
	}
}

func mib(n int64) string {
	return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
}

// s3Flags are the S3 target flags shared by backup create and backuppolicy
// create.
type s3Flags struct {
	endpoint, bucket, prefix, region, credentials, encryptionKey, immutableFor string
	virtualHost                                                                bool
}

func (f *s3Flags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.endpoint, "s3-endpoint", "", "S3 endpoint URL, e.g. http://minio.backup:9000 (selects an incremental-capable S3 backup)")
	fs.StringVar(&f.bucket, "s3-bucket", "", "S3 bucket")
	fs.StringVar(&f.prefix, "s3-prefix", "", "key prefix in front of <namespace>/<machine>")
	fs.StringVar(&f.region, "s3-region", "", "S3 region (default us-east-1)")
	fs.StringVar(&f.credentials, "s3-credentials", "", "Secret with accessKeyId/secretAccessKey, labelled "+model.BackupCredentialsLabel+"=true")
	fs.BoolVar(&f.virtualHost, "s3-virtual-host", false, "address bucket.endpoint/key instead of endpoint/bucket/key")
	fs.StringVar(&f.encryptionKey, "encryption-key", "", "Secret whose \"key\" is a 32-byte AES-256-GCM key (raw or base64), labelled "+model.BackupCredentialsLabel+"=true")
	fs.StringVar(&f.immutableFor, "immutable-for", "", "S3 Object Lock retention, e.g. 30d or 720h (the bucket needs Object Lock enabled)")
}

func (f *s3Flags) set() bool {
	return f.endpoint != "" || f.bucket != "" || f.credentials != ""
}

func (f *s3Flags) target() *model.BackupTarget {
	t := &model.S3BackupTarget{Endpoint: f.endpoint, Bucket: f.bucket, Prefix: f.prefix, Region: f.region, CredentialsSecretRef: f.credentials}
	if f.virtualHost {
		pathStyle := false
		t.PathStyle = &pathStyle
	}
	return &model.BackupTarget{S3: t}
}

func (f *s3Flags) encryption() *model.BackupEncryption {
	if f.encryptionKey == "" {
		return nil
	}
	return &model.BackupEncryption{Algorithm: model.BackupEncryptionAES, KeySecretRef: f.encryptionKey}
}

func newBackup(ns, machine, name, quiesce string, atlasSpec *model.MachineBackupAtlas) model.MachineBackup {
	if name == "" {
		name = resourceName(machine + "-" + time.Now().UTC().Format("20060102-150405"))
	}
	return model.MachineBackup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackup},
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Spec:     model.MachineBackupSpec{MachineName: machine, Quiesce: quiesce, Atlas: atlasSpec},
	}
}

// inheritCBTTarget fetches the policy or parent an S3 backup takes its
// target from; see model.InheritBackupTarget.
func inheritCBTTarget(ctx context.Context, kc *kube.Client, b *model.MachineBackup, policy string) error {
	var pol *model.MachineBackupPolicy
	var parent *model.MachineBackup
	if policy != "" {
		p, err := kc.GetMachineBackupPolicy(ctx, b.Namespace(), policy)
		if err != nil {
			return fmt.Errorf("get MachineBackupPolicy %s: %w", policy, err)
		}
		pol = &p
	} else if b.Spec.Target == nil && b.Spec.Parent != "" {
		p, err := kc.GetMachineBackup(ctx, b.Namespace(), b.Spec.Parent)
		if err != nil {
			return fmt.Errorf("get parent MachineBackup %s: %w", b.Spec.Parent, err)
		}
		parent = &p
	}
	return model.InheritBackupTarget(b, pol, parent)
}

func submitBackup(ctx context.Context, kc *kube.Client, b model.MachineBackup) (model.MachineBackup, error) {
	if !model.ValidBackupQuiesce(b.Spec.Quiesce) {
		return model.MachineBackup{}, fmt.Errorf("quiesce must be auto, required or never")
	}
	if err := model.ValidateCBTBackup(b); err != nil {
		return model.MachineBackup{}, err
	}
	return kc.CreateMachineBackup(ctx, b.Namespace(), b)
}

func createBackupRestore(ctx context.Context, kc *kube.Client, ns, backup, name, machine, storageClass string) (model.MachineBackupRestore, error) {
	if name == "" {
		name = resourceName(backup + "-restore-" + time.Now().UTC().Format("20060102-150405"))
	}
	return kc.CreateMachineBackupRestore(ctx, ns, model.MachineBackupRestore{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackupRestore},
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Spec:     model.MachineBackupRestoreSpec{BackupName: backup, MachineName: machine, StorageClassName: storageClass},
	})
}

// describeBackup prints the MachineBackup as JSON and, for an S3 backup,
// the chain it belongs to with stored and changed bytes per point.
func describeBackup(ctx context.Context, kc *kube.Client, ns, name string) {
	b, err := kc.GetMachineBackup(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	out, _ := json.MarshalIndent(b, "", "  ")
	fmt.Println(string(out))
	if !b.IsCBT() || b.Status.Chain == nil {
		return
	}
	items, err := kc.ListMachineBackupsNamespace(ctx, ns)
	if err != nil {
		fatal(err)
	}
	fmt.Println("\nChain:")
	for _, c := range model.BackupChains(items, b.Spec.MachineName) {
		if c.ID == b.Status.Chain.ID {
			printBackupChains(os.Stdout, []model.BackupChainView{c})
		}
	}
}
