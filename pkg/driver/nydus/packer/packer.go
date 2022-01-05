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
	"fmt"
	"io/ioutil"
	"os"
	"path"

	"github.com/containerd/containerd/content"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/goharbor/acceleration-service/pkg/driver/nydus/builder"
	"github.com/goharbor/acceleration-service/pkg/driver/nydus/utils"
)

type Descriptor struct {
	Blob      *ocispec.Descriptor
	Bootstrap ocispec.Descriptor
}

type Packer struct {
	parentWorkDir string
	builderPath   string
	sg            singleflight.Group
}

func New(parentWorkDir, builderPath string) (*Packer, error) {
	return &Packer{
		parentWorkDir: parentWorkDir,
		builderPath:   builderPath,
	}, nil
}

func (p *Packer) prepareWorkdir() (string, func() error, error) {
	workDir, err := ioutil.TempDir(p.parentWorkDir, "nydus-build-")
	if err != nil {
		return "", nil, errors.Wrapf(err, "create work dir")
	}

	// Create a directory to store nydus blob file for every layer.
	blobDirPath := path.Join(workDir, "blobs")
	if err := os.MkdirAll(blobDirPath, 0755); err != nil {
		return "", nil, errors.Wrapf(err, "create blob dir %s", blobDirPath)
	}

	// Create a directory to store nydus bootstrap file for every layer.
	bootstrapDirPath := path.Join(workDir, "bootstraps")
	if err := os.MkdirAll(bootstrapDirPath, 0755); err != nil {
		return "", nil, errors.Wrapf(err, "create bootstrap dir %s", bootstrapDirPath)
	}

	cleanup := func() error {
		return os.RemoveAll(workDir)
	}

	return workDir, cleanup, nil
}

func (p *Packer) diffBuild(ctx context.Context, workDir string, layers []*BuildLayer, diffSkip *int) (*builder.Output, error) {
	diffPaths := []string{}
	diffHintPaths := []string{}

	bootstrapDir := path.Join(workDir, "bootstraps")
	blobDir := path.Join(workDir, "blobs")
	outputJSONPath := path.Join(workDir, "output.json")

	for _, layer := range layers {
		diffPaths = append(diffPaths, layer.diffPath)
		diffHintPaths = append(diffHintPaths, layer.diffHintPath)
	}

	var parentBootstrapPath *string
	if diffSkip != nil {
		// Found nydus bootstrap cache, unpack targz and use it as parent bootstrap.
		_parentBootstrapPath := path.Join(bootstrapDir, "parent-bootstrap")
		parentBootstrapPath = &_parentBootstrapPath
		layer := layers[*diffSkip]
		cachedBootstrap, _ := layer.GetCache(ctx, CompressionTypeBootstrap)
		if cachedBootstrap == nil {
			return nil, fmt.Errorf("can't find bootstrap cache")
		}
		ra, err := layer.ContentStore(ctx).ReaderAt(ctx, ocispec.Descriptor{
			Digest: cachedBootstrap.Digest,
			Size:   cachedBootstrap.Size,
		})
		if err != nil {
			return nil, errors.Wrap(err, "read bootstrap from content store")
		}
		defer ra.Close()

		cr := content.NewReader(ra)
		if err := utils.UnpackFile(cr, utils.BootstrapFileNameInLayer, _parentBootstrapPath); err != nil {
			return nil, errors.Wrap(err, "unpack nydus bootstrap")
		}
	}

	build := builder.New(p.builderPath)
	output, err := build.Run(builder.Option{
		BootstrapDirPath:    bootstrapDir,
		BlobDirPath:         blobDir,
		ParentBootstrapPath: parentBootstrapPath,

		DiffLayerPaths:     diffPaths,
		DiffHintLayerPaths: diffHintPaths,
		DiffSkipLayer:      diffSkip,

		OutputJSONPath: outputJSONPath,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "build layers %v %v", diffPaths, diffHintPaths)
	}

	return output, nil
}

func (p *Packer) Build(ctx context.Context, layers []SourceLayer) ([]Descriptor, error) {
	var diffSkip *int
	var parent *BuildLayer

	descs := make([]Descriptor, len(layers))

	// Find cache first, to skip layers that have been built.
	buildLayers := []*BuildLayer{}
	for idx := range layers {
		layer := layers[idx]

		// Find the layer cache.
		cachedBootstrapDesc, _ := layer.GetCache(ctx, CompressionTypeBootstrap)
		cachedBlobDesc, err := layer.GetCache(ctx, CompressionTypeBlob)
		if cachedBootstrapDesc != nil && err == nil {
			descs[idx] = Descriptor{
				Blob:      cachedBlobDesc,
				Bootstrap: *cachedBootstrapDesc,
			}
			if parent == nil || diffSkip != nil {
				_idx := idx
				diffSkip = &_idx
			}
		}

		if diffSkip != nil {
			// All cache hit, skip following mount and build.
			if *diffSkip == len(layers)-1 {
				return descs, nil
			}
		}

		buildLayer := BuildLayer{
			SourceLayer: layer,
			parent:      parent,
		}
		parent = &buildLayer

		buildLayers = append(buildLayers, &buildLayer)
	}

	defer func() {
		// Release all layer mounts.
		for idx := range buildLayers {
			buildLayers[idx].umount(ctx)
		}
	}()

	// Mount all source layers.
	mountEg, mountCtx := errgroup.WithContext(ctx)
	for idx := range buildLayers {
		mountEg.Go(func(idx int) func() error {
			return func() error {
				if err := buildLayers[idx].mount(mountCtx); err != nil {
					return errors.Wrap(err, "layer mount")
				}
				return nil
			}
		}(idx))
	}
	if err := mountEg.Wait(); err != nil {
		return nil, errors.Wrap(err, "export all nydus blobs")
	}
	for idx := range buildLayers {
		layer := buildLayers[idx]
		if err := layer.mountWithLower(ctx); err != nil {
			return nil, errors.Wrap(err, "mount with lower layer")
		}
	}

	// Prepare work directory, the blobs and bootstraps of nydus will
	// be written into the directory.
	workDir, cleanup, err := p.prepareWorkdir()
	if err != nil {
		return nil, errors.Wrap(err, "prepare work directory")
	}
	defer func() {
		if err := cleanup(); err != nil {
			logrus.WithError(err).Warnf("failed to cleanup work dir %s", workDir)
		}
	}()

	// Call nydus builder to build, skip the layer specified by `diffSkip`.
	output, err := p.diffBuild(ctx, workDir, buildLayers, diffSkip)
	if err != nil {
		return nil, errors.Wrap(err, "diff build with nydus")
	}

	// The base is the first index of layer to build after skipping cache.
	base := 0
	if diffSkip != nil {
		base = *diffSkip + 1
	}

	// Export nydus blobs to content store.
	exportEg, ctx := errgroup.WithContext(ctx)
	for idx := range output.OrderedBlobs {
		exportEg.Go(func(idx int) func() error {
			return func() error {
				blob := output.OrderedBlobs[idx]

				// Skip to export empty nydus blob.
				if blob == nil {
					layer := buildLayers[idx]
					if err := layer.SetCache(ctx, CompressionTypeBlob, nil); err != nil {
						return errors.Wrap(err, "set nydus blob cache")
					}
					return nil
				}

				// Skip to export cached nydus blobs.
				if idx < base {
					return nil
				}

				blobPath := path.Join(workDir, "blobs", blob.BlobID)
				layer := buildLayers[idx]

				// Use singleflight to deduplicate the export of same blob.
				_desc, err, _ := p.sg.Do(blob.BlobID, func() (interface{}, error) {
					return layer.exportBlob(ctx, blobPath)
				})
				if err != nil {
					return errors.Wrap(err, "export nydus blob")
				}

				desc := _desc.(*ocispec.Descriptor)
				if err := layer.SetCache(ctx, CompressionTypeBlob, desc); err != nil {
					return errors.Wrap(err, "set nydus blob cache")
				}
				descs[idx].Blob = desc

				return nil
			}
		}(idx))
	}

	if len(output.Bootstraps) <= 0 {
		return nil, fmt.Errorf("can't find valid nydus bootstrap")
	}

	// Export nydus bootstraps to content store.
	for idx, bootstrapName := range output.Bootstraps {
		idx := base + idx
		bootstrapPath := path.Join(workDir, "bootstraps", bootstrapName)
		exportEg.Go(func(idx int) func() error {
			return func() error {
				layer := buildLayers[idx]
				desc, err := layer.exportBootstrap(ctx, &p.sg, bootstrapPath)
				if err != nil {
					return errors.Wrap(err, "export nydus blob")
				}
				if err := layer.SetCache(ctx, CompressionTypeBootstrap, desc); err != nil {
					return errors.Wrap(err, "set nydus bootstrap cache")
				}
				descs[idx].Bootstrap = *desc
				return nil
			}
		}(idx))
	}

	// Wait to export all nydus blobs/bootstraps.
	if err := exportEg.Wait(); err != nil {
		return nil, errors.Wrap(err, "export all nydus blobs")
	}

	return descs, nil
}
