/*
 * Copyright (c) 2026. Nydus Developers. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package snapshot

import (
	"context"
	"maps"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	snpkg "github.com/containerd/containerd/v2/pkg/snapshotters"
	"github.com/containerd/errdefs"
	"github.com/containerd/nydus-snapshotter/pkg/label"
	"github.com/containerd/nydus-snapshotter/pkg/snapshot"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

func (o *snapshotter) findIndexAlternativeLayer(ctx context.Context, key string) (string, snapshots.Info, error) {
	id, info, err := snapshot.IterateParentSnapshots(ctx, o.ms, key, func(_ string, i snapshots.Info) bool {
		// Persisted placeholders must not become ordinary empty directories when
		// index detection is disabled or their registry is unavailable.
		return i.Labels[label.NydusIndexAlternative] == "true" || o.fs.CheckIndexAlternative(ctx, i.Labels)
	})
	if err != nil {
		return "", snapshots.Info{}, err
	}

	// containerd supplies the current layer and all subsequent layers in
	// cri.image-layers. Only the final layer can represent the full bootstrap.
	// Stop at an invalid candidate: searching further would discard skipped
	// layers between it and an earlier complete image.
	layer := info.Labels[snpkg.TargetLayerDigestLabel]
	if digest.Digest(layer).Validate() != nil || info.Labels[snpkg.TargetImageLayersLabel] != layer {
		return "", snapshots.Info{}, errors.Wrapf(errdefs.ErrFailedPrecondition,
			"index alternative snapshot %q for image %q is not a verified top layer; refusing to mount the full image bootstrap for a shared prefix",
			info.Name, info.Labels[snpkg.TargetRefLabel])
	}
	if !o.fs.IndexDetectEnabled() {
		return "", snapshots.Info{}, errors.Wrapf(errdefs.ErrFailedPrecondition,
			"snapshot %q requires index detection; its OCI layers were skipped", info.Name)
	}
	return id, info, nil
}

// mountIndexAlternative is also used when unpacking ordinary OCI children and
// preparing views, both of which need the same complete parent rootfs as a
// container. The caller must validate the candidate before fetching metadata or
// reusing an existing RAFS instance.
func (o *snapshotter) mountIndexAlternative(ctx context.Context, s storage.Snapshot, key, id string, info snapshots.Info, consumerLabels map[string]string) ([]mount.Mount, error) {
	metaPath := filepath.Join(o.snapshotDir(id), "fs", "image.boot")
	if err := o.fs.TryFetchMetadataFromIndex(ctx, info.Labels, metaPath); err != nil {
		return nil, errors.Wrap(err, "fetch index alternative metadata")
	}
	labels := maps.Clone(info.Labels)
	for _, k := range []string{label.SnapshotUIDMapping, label.SnapshotGIDMapping} {
		delete(labels, k)
		if v, ok := consumerLabels[k]; ok {
			labels[k] = v
		}
	}
	if err := o.fs.Mount(ctx, id, labels, &s); err != nil {
		return nil, err
	}
	if err := o.fs.WaitUntilReady(id, labels); err != nil {
		return nil, err
	}
	return o.mountRemote(ctx, consumerLabels, s, id, key)
}
