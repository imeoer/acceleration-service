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

package nydus

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/nydus-snapshotter/pkg/backend"
	nydusify "github.com/containerd/nydus-snapshotter/pkg/converter"
	"github.com/goharbor/acceleration-service/pkg/driver/nydus/parser"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"

	accelcontent "github.com/goharbor/acceleration-service/pkg/content"
	nydusutils "github.com/goharbor/acceleration-service/pkg/driver/nydus/utils"
	"github.com/goharbor/acceleration-service/pkg/utils"
)

type chunkDictInfo struct {
	BootstrapPath string
}

type Driver struct {
	workDir       string
	builderPath   string
	fsVersion     string
	compressor    string
	chunkDictRef  string
	mergeManifest bool
	backend       backend.Backend
}

func New(cfg map[string]string) (*Driver, error) {
	workDir := cfg["work_dir"]
	if workDir == "" {
		workDir = os.TempDir()
	}

	builderPath := cfg["builder"]
	if builderPath == "" {
		builderPath = "nydus-image"
	}

	chunkDictRef := cfg["chunk_dict_ref"]

	var err error
	var _backend backend.Backend
	backendType := cfg["backend_type"]
	backendConfig := cfg["backend_config"]
	if backendType != "" && backendConfig != "" {
		_backend, err = backend.NewBackend(backendType, []byte(backendConfig))
		if err != nil {
			return nil, errors.Wrap(err, "create blob backend")
		}
	}

	fsVersion := cfg["fs_version"]
	if fsVersion == "" {
		// For compatibility of older configuration.
		fsVersion = cfg["rafs_version"]
		if fsVersion == "" {
			fsVersion = "5"
		}
	}
	compressor := cfg["compressor"]
	if compressor == "" {
		// For compatibility of older configuration.
		compressor = cfg["rafs_compressor"]
	}

	_mergeManifest := cfg["merge_manifest"]
	mergeManifest := false
	if _mergeManifest != "" {
		mergeManifest, err = strconv.ParseBool(_mergeManifest)
		if err != nil {
			return nil, fmt.Errorf("invalid merge_manifest option")
		}
	}

	return &Driver{
		workDir:       workDir,
		builderPath:   builderPath,
		fsVersion:     fsVersion,
		compressor:    compressor,
		chunkDictRef:  chunkDictRef,
		mergeManifest: mergeManifest,
		backend:       _backend,
	}, nil
}

func (d *Driver) Name() string {
	return "nydus"
}

func (d *Driver) Version() string {
	return ""
}

func (d *Driver) Convert(ctx context.Context, provider accelcontent.Provider) (*ocispec.Descriptor, error) {
	desc, err := d.convert(ctx, provider)
	if err != nil {
		return nil, err
	}
	if d.mergeManifest {
		return d.makeManifestIndex(ctx, provider.ContentStore(), provider.Image().Target(), *desc)
	}
	return desc, err
}

func (d *Driver) convert(ctx context.Context, provider accelcontent.Provider) (*ocispec.Descriptor, error) {
	cs := provider.ContentStore()

	chunkDictPath := ""
	if d.chunkDictRef != "" {
		chunkDictInfo, err := d.getChunkDict(ctx, provider)
		if err != nil {
			return nil, errors.Wrap(err, "get chunk dict info")
		}
		chunkDictPath = chunkDictInfo.BootstrapPath
	}

	opt := nydusify.PackOption{
		FsVersion:     d.fsVersion,
		Compressor:    d.compressor,
		BuilderPath:   d.builderPath,
		WorkDir:       d.workDir,
		ChunkDictPath: chunkDictPath,
		Backend:       d.backend,
	}

	newDesc, err := converter.IndexConvertFuncWithHook(nydusify.LayerConvertFunc(opt), true, platforms.All, converter.ConvertHooks{
		PostConvertHook: nydusify.ConvertHookFunc(opt),
	})(
		ctx, cs, provider.Image().Target(),
	)
	if err != nil {
		return nil, errors.Wrap(err, "convert to nydus image")
	}

	return newDesc, nil
}

func (d *Driver) makeManifestIndex(ctx context.Context, cs content.Store, oci, nydus ocispec.Descriptor) (*ocispec.Descriptor, error) {
	ociDescs, err := utils.GetManifests(ctx, cs, oci)
	if err != nil {
		return nil, errors.Wrap(err, "get oci image manifest list")
	}

	nydusDescs, err := utils.GetManifests(ctx, cs, nydus)
	if err != nil {
		return nil, errors.Wrap(err, "get nydus image manifest list")
	}
	for idx, desc := range nydusDescs {
		if desc.Platform == nil {
			desc.Platform = &ocispec.Platform{}
		}
		desc.Platform.OSFeatures = []string{nydusutils.ManifestOSFeatureNydus}
		nydusDescs[idx] = desc
	}

	descs := append(ociDescs, nydusDescs...)

	index := ocispec.Index{
		Versioned: specs.Versioned{
			SchemaVersion: 2,
		},
		Manifests: descs,
	}

	indexDesc, indexBytes, err := nydusutils.MarshalToDesc(index, ocispec.MediaTypeImageIndex)
	if err != nil {
		return nil, errors.Wrap(err, "marshal image manifest index")
	}

	labels := map[string]string{}
	for idx, desc := range descs {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.%d", idx)] = desc.Digest.String()
	}
	if err := content.WriteBlob(
		ctx, cs, indexDesc.Digest.String(), bytes.NewReader(indexBytes), *indexDesc, content.WithLabels(labels),
	); err != nil {
		return nil, errors.Wrap(err, "write image manifest")
	}

	return indexDesc, nil
}

func (d *Driver) getChunkDict(ctx context.Context, provider accelcontent.Provider) (*chunkDictInfo, error) {
	if d.chunkDictRef == "" {
		return nil, nil
	}

	parser, err := parser.New(provider)
	if err != nil {
		return nil, errors.Wrap(err, "create chunk dict parser")
	}
	bootstrapReader, _, err := parser.PullAsChunkDict(ctx, d.chunkDictRef)
	if err != nil {
		return nil, errors.Wrapf(err, "pull chunk dict image %s", d.chunkDictRef)
	}
	defer bootstrapReader.Close()

	bootstrapFile, err := os.CreateTemp(d.workDir, "nydus-chunk-dict-")
	if err != nil {
		return nil, errors.Wrapf(err, "create temp file for chunk dict bootstrap")
	}
	defer bootstrapFile.Close()

	bootstrapPath := bootstrapFile.Name()
	// FIXME: avoid unpacking the bootstrap on every conversion.
	if err := nydusutils.UnpackFile(content.NewReader(bootstrapReader), nydusutils.BootstrapFileNameInLayer, bootstrapFile.Name()); err != nil {
		return nil, errors.Wrap(err, "unpack nydus bootstrap")
	}

	chunkDict := chunkDictInfo{
		BootstrapPath: bootstrapPath,
	}

	return &chunkDict, nil
}
