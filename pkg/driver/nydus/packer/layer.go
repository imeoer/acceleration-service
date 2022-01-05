// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package packer

import (
	"context"
	"os"
	"path"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/mount"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"

	"github.com/goharbor/acceleration-service/pkg/driver/nydus/utils"
)

type CompressionType = string

const (
	CompressionTypeBlob      = "nydus-blob"
	CompressionTypeBootstrap = "nydus-bootstrap"
)

type SourceLayer interface {
	// ContentStore provides containerd content store for nydus layer export.
	ContentStore(ctx context.Context) content.Store

	// Mount mounts layer by snapshotter, release func provides a unmount operation.
	Mount(ctx context.Context) (mounts []mount.Mount, release func() error, err error)

	// SetCache records nydus bootstrap/blob descriptor to cache, desc == nil if
	// nydus blob content is empty.
	SetCache(ctx context.Context, compressionType CompressionType, desc *ocispec.Descriptor) error

	// GetCache get nydus bootstrap/blob descriptor from cache, following situations
	// should be handled:
	// err != nil, cache miss;
	// err == nil:
	//   - desc == nil, cache hints, but nydus blob content is empty;
	//   - desc != nil, cache hints;
	GetCache(ctx context.Context, compressionType CompressionType) (desc *ocispec.Descriptor, err error)
}

type BuildLayer struct {
	SourceLayer

	parent *BuildLayer

	mounts       []mount.Mount
	release      func() error
	mountRelease chan bool

	diffPath     string
	diffHintPath string
}

func (layer *BuildLayer) mount(ctx context.Context) error {
	mounts, release, err := layer.Mount(ctx)
	if err != nil {
		return errors.Wrap(err, "layer mount")
	}

	layer.mounts = mounts
	layer.release = release

	return nil
}

func (layer *BuildLayer) umount(ctx context.Context) {
	if layer.mounts == nil {
		return
	}

	close(layer.mountRelease)
	layer.release()
	layer.mounts = nil
}

func (layer *BuildLayer) mountWithLower(ctx context.Context) error {
	mountDone := make(chan error)
	layer.mountRelease = make(chan bool)

	lower := []mount.Mount{}
	upper := layer.mounts
	if layer.parent != nil {
		lower = layer.parent.mounts
	}

	go func() {
		if err := mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
			return mount.WithTempMount(ctx, upper, func(upperRoot string) error {
				// FIXME: for non-overlay snapshotter, we can't use diff hint feature,
				// need fallback to non-hint mode.
				upperSnapshot, err := GetUpperdir(lower, upper)
				if err != nil {
					err = errors.Wrap(err, "get upper directory from mount")
					mountDone <- err
					return err
				}
				layer.diffPath = upperRoot
				layer.diffHintPath = upperSnapshot
				mountDone <- nil
				<-layer.mountRelease
				return nil
			})
		}); err != nil {
			mountDone <- errors.Wrap(err, "mount with temp")
		}
	}()

	err := <-mountDone

	return err
}

func (layer *BuildLayer) exportBlob(ctx context.Context, blobPath string) (*ocispec.Descriptor, error) {
	blobFile, err := os.Open(blobPath)
	if err != nil {
		return nil, errors.Wrapf(err, "open blob %s", blobPath)
	}
	defer blobFile.Close()

	blobStat, err := blobFile.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "stat blob %s", blobPath)
	}

	blobID := path.Base(blobPath)
	blobDigest := digest.NewDigestFromEncoded(digest.SHA256, blobID)
	desc := ocispec.Descriptor{
		Digest:    blobDigest,
		Size:      blobStat.Size(),
		MediaType: utils.MediaTypeNydusBlob,
		Annotations: map[string]string{
			// Use `containerd.io/uncompressed` to generate DiffID of
			// layer defined in OCI spec.
			utils.LayerAnnotationUncompressed: blobDigest.String(),
			utils.LayerAnnotationNydusBlob:    "true",
		},
	}

	// FIXME: find a efficient way to use fifo to pipe blob data from builder to content store.
	cs := layer.ContentStore(ctx)
	if err := content.WriteBlob(
		ctx, cs, desc.Digest.String(), blobFile, desc,
	); err != nil {
		return nil, errors.Wrapf(err, "export blob %s to content store", blobPath)
	}

	return &desc, nil
}

func (layer *BuildLayer) exportBootstrap(ctx context.Context, sg *singleflight.Group, bootstrapPath string) (*ocispec.Descriptor, error) {
	bootstrapFile, err := os.Open(bootstrapPath)
	if err != nil {
		return nil, errors.Wrapf(err, "open bootstrap %s", bootstrapPath)
	}
	defer bootstrapFile.Close()

	compressedDigest, compressedSize, err := utils.PackTargzInfo(
		bootstrapPath, utils.BootstrapFileNameInLayer, true,
	)
	if err != nil {
		return nil, errors.Wrap(err, "calculate compressed boostrap digest")
	}

	_desc, err, _ := sg.Do(compressedDigest.String(), func() (interface{}, error) {
		uncompressedDigest, _, err := utils.PackTargzInfo(
			bootstrapPath, utils.BootstrapFileNameInLayer, false,
		)
		if err != nil {
			return nil, errors.Wrap(err, "calculate uncompressed boostrap digest")
		}

		desc := ocispec.Descriptor{
			Digest:    compressedDigest,
			Size:      compressedSize,
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Annotations: map[string]string{
				// Use `containerd.io/uncompressed` to generate DiffID of
				// layer defined in OCI spec.
				utils.LayerAnnotationUncompressed:   uncompressedDigest.String(),
				utils.LayerAnnotationNydusBootstrap: "true",
			},
		}

		reader, err := utils.PackTargz(bootstrapPath, utils.BootstrapFileNameInLayer, true)
		if err != nil {
			return nil, errors.Wrap(err, "pack bootstrap to tar.gz")
		}
		defer reader.Close()

		cs := layer.ContentStore(ctx)
		if err := content.WriteBlob(
			ctx, cs, desc.Digest.String(), reader, desc,
		); err != nil {
			return nil, errors.Wrapf(err, "write bootstrap %s to content store", bootstrapPath)
		}

		return &desc, nil
	})
	if err != nil {
		return nil, err
	}

	return _desc.(*ocispec.Descriptor), nil
}
