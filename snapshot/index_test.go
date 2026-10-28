//go:build linux

/*
 * Copyright (c) 2026. Nydus Developers. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	snpkg "github.com/containerd/containerd/v2/pkg/snapshotters"
	"github.com/containerd/errdefs"
	"github.com/containerd/nydus-snapshotter/pkg/filesystem"
	"github.com/containerd/nydus-snapshotter/pkg/index"
	"github.com/containerd/nydus-snapshotter/pkg/label"
	"github.com/containerd/nydus-snapshotter/pkg/rafs"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func indexLayerLabels(layer string, subsequent ...string) map[string]string {
	layerDigest := digest.FromString(layer).String()
	layers := layerDigest
	for _, l := range subsequent {
		layers += "," + digest.FromString(l).String()
	}
	return map[string]string{
		label.NydusIndexAlternative:     "true",
		snpkg.TargetRefLabel:            "registry.invalid/repro:a",
		snpkg.TargetManifestDigestLabel: digest.FromString("manifest-a").String(),
		snpkg.TargetLayerDigestLabel:    layerDigest,
		snpkg.TargetImageLayersLabel:    layers,
	}
}

func enableIndexDetection(t *testing.T, sn *snapshotter) {
	t.Helper()
	require.NoError(t, filesystem.WithIndexManager(index.NewManager(false))(sn.fs))
}

// Use the real metadata store and mount descriptors without a registry, mount(2)
// or nydusd. An existing bootstrap/RAFS instance also models an affected node
// upgraded from the version which mounted the wrong full image at this snapshot.
func cacheIndexRootfs(t *testing.T, sn *snapshotter, id string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(sn.upperPath(id), "image.boot"), []byte("cached bootstrap"), 0644))
	rootfs := filepath.Join(sn.snapshotDir(id), "mnt")
	require.NoError(t, os.MkdirAll(rootfs, 0755))
	rafs.RafsGlobalCache.Add(&rafs.Rafs{SnapshotID: id, FsDriver: "test-cache-only", Mountpoint: rootfs})
	return rootfs
}

func TestIndexAlternativeRejectsSharedPrefix(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "enabled"
		if !enabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			for _, entry := range []string{"prepare", "unpack", "view", "mounts-active", "mounts-view", "mounts-committed"} {
				t.Run(entry, func(t *testing.T) {
					sn := newDockerChainTestSnapshotter(t)
					if enabled {
						enableIndexDetection(t, sn)
					}
					ctx := context.Background()
					createCommittedDockerChainLayer(t, sn, "shared-1", "", indexLayerLabels("shared-1", "shared-2", "a-top"))
					shared := createCommittedDockerChainLayer(t, sn, "shared-2", "shared-1", indexLayerLabels("shared-2", "a-top"))
					createCommittedDockerChainLayer(t, sn, "a-top", "shared-2", indexLayerLabels("a-top"))
					createCommittedDockerChainLayer(t, sn, "b-top", "shared-2", nil)
					cacheIndexRootfs(t, sn, shared.ID)

					var mounts []mount.Mount
					var err error
					switch entry {
					case "prepare":
						mounts, err = sn.Prepare(ctx, "container-b", "b-top")
					case "unpack":
						mounts, err = sn.Prepare(ctx, "unpack-b", "shared-2", snapshots.WithLabels(map[string]string{
							label.TargetSnapshotRef: "b-layer",
						}))
					case "view":
						mounts, err = sn.View(ctx, "view-b", "b-top")
					case "mounts-active", "mounts-view":
						kind := snapshots.KindActive
						if entry == "mounts-view" {
							kind = snapshots.KindView
						}
						_, _, err = sn.createSnapshot(ctx, kind, "existing-b", "b-top", nil)
						require.NoError(t, err)
						mounts, err = sn.Mounts(ctx, "existing-b")
					case "mounts-committed":
						mounts, err = sn.Mounts(ctx, "shared-2")
					}
					require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
					require.ErrorContains(t, err, "shared prefix")
					require.Nil(t, mounts, "must not expose either the full A rootfs or empty native lowerdirs")
				})
			}
		})
	}
}

func TestIndexAlternativeRequiresVerifiedTopLayer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"missing layer", func(l map[string]string) { delete(l, snpkg.TargetLayerDigestLabel) }},
		{"missing suffix", func(l map[string]string) { delete(l, snpkg.TargetImageLayersLabel) }},
		{"invalid digest", func(l map[string]string) {
			l[snpkg.TargetLayerDigestLabel], l[snpkg.TargetImageLayersLabel] = "invalid", "invalid"
		}},
		{"mismatched digest", func(l map[string]string) { l[snpkg.TargetImageLayersLabel] = digest.FromString("other").String() }},
		{"intermediate", func(l map[string]string) { l[snpkg.TargetImageLayersLabel] += "," + digest.FromString("top").String() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sn := newDockerChainTestSnapshotter(t)
			enableIndexDetection(t, sn)
			// A valid earlier image must not hide a later skipped intermediate layer.
			createCommittedDockerChainLayer(t, sn, "base-top", "", indexLayerLabels("base-top"))
			labels := indexLayerLabels("candidate")
			tc.mutate(labels)
			createCommittedDockerChainLayer(t, sn, "candidate", "base-top", labels)
			id, _, err := sn.findIndexAlternativeLayer(context.Background(), "candidate")
			require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
			require.Empty(t, id)
		})
	}
}

func TestIndexAlternativeFullImageAndOCIChild(t *testing.T) {
	for _, child := range []bool{false, true} {
		name := "image-a"
		if child {
			name = "image-c"
		}
		t.Run(name, func(t *testing.T) {
			sn := newDockerChainTestSnapshotter(t)
			enableIndexDetection(t, sn)
			ctx := context.Background()
			createCommittedDockerChainLayer(t, sn, "shared", "", indexLayerLabels("shared", "a-top"))
			top := createCommittedDockerChainLayer(t, sn, "a-top", "shared", indexLayerLabels("a-top"))
			rootfs := cacheIndexRootfs(t, sn, top.ID)
			parent := "a-top"
			want := []string{rootfs}
			if child {
				c := createCommittedDockerChainLayer(t, sn, "c-top", parent, nil)
				parent = "c-top"
				want = append([]string{sn.upperPath(c.ID)}, want...)
			}
			prepared, err := sn.Prepare(ctx, "container", parent)
			require.NoError(t, err)
			assert.Equal(t, want, dockerChainLowerdir(t, prepared))
			mounted, err := sn.Mounts(ctx, "container")
			require.NoError(t, err)
			assert.Equal(t, prepared, mounted)

			// Applying a child tar must also see the complete parent filesystem.
			unpack, err := sn.Prepare(ctx, "unpack", parent, snapshots.WithLabels(map[string]string{
				label.TargetSnapshotRef: "new-oci-layer",
			}))
			require.NoError(t, err)
			assert.Equal(t, want, dockerChainLowerdir(t, unpack))

			view, err := sn.View(ctx, "view", parent)
			require.NoError(t, err)
			viewMounts, err := sn.Mounts(ctx, "view")
			require.NoError(t, err)
			assert.Equal(t, view, viewMounts)
			lower := dockerChainLowerdir(t, view)
			require.Len(t, lower, len(want)+1)
			assert.Equal(t, want, lower[:len(want)])
		})
	}
}

func TestIndexAlternativeDisabledRejectsPersistedTop(t *testing.T) {
	sn := newDockerChainTestSnapshotter(t)
	createCommittedDockerChainLayer(t, sn, "a-top", "", indexLayerLabels("a-top"))
	mounts, err := sn.Prepare(context.Background(), "container", "a-top")
	require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	require.ErrorContains(t, err, "requires index detection")
	require.Nil(t, mounts)
}

func TestIndexAlternativeStillSkipsImageLayers(t *testing.T) {
	sn := newDockerChainTestSnapshotter(t)
	enableIndexDetection(t, sn)
	ctx := context.Background()
	parent := ""
	for _, name := range []string{"base", "top"} {
		labels := indexLayerLabels(name)
		if name == "base" {
			labels = indexLayerLabels(name, "top")
		}
		labels[label.TargetSnapshotRef] = name
		mounts, err := sn.Prepare(ctx, name+"-unpack", parent, snapshots.WithLabels(labels))
		require.ErrorIs(t, err, errdefs.ErrAlreadyExists)
		require.Nil(t, mounts)
		info, err := sn.Stat(ctx, name)
		require.NoError(t, err)
		assert.Equal(t, snapshots.KindCommitted, info.Kind)
		assert.Equal(t, "true", info.Labels[label.NydusIndexAlternative])
		parent = name
	}
}

func TestIndexAlternativeOrdinaryOCIStaysNative(t *testing.T) {
	sn := newDockerChainTestSnapshotter(t)
	enableIndexDetection(t, sn)
	base := createCommittedDockerChainLayer(t, sn, "base", "", nil)
	mounts, err := sn.Prepare(context.Background(), "container", "base")
	require.NoError(t, err)
	assert.Equal(t, []string{sn.upperPath(base.ID)}, dockerChainLowerdir(t, mounts))
	_, _, err = sn.findIndexAlternativeLayer(context.Background(), "base")
	require.ErrorIs(t, err, errdefs.ErrNotFound)
}
