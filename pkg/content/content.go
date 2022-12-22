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

package content

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/remotes"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	nydusUtils "github.com/goharbor/acceleration-service/pkg/driver/nydus/utils"
	"github.com/goharbor/acceleration-service/pkg/remote"
	"github.com/goharbor/acceleration-service/pkg/utils"
)

var logger = logrus.WithField("module", "content")

// Provider provides necessary image utils, image content
// store for image conversion.
type Provider interface {
	// Use plain HTTP to communicate with registry.
	UsePlainHTTP()
	// Resolve attempts to resolve the reference into a name and descriptor.
	Resolver(ref string) (remotes.Resolver, error)
	// Pull pulls source image from remote registry by specified reference.
	// This pulls all platforms of the image but Image() returns containerd.Image for
	// the default platform.
	Pull(ctx context.Context, ref string) error
	// Push pushes target image to remote registry by specified reference,
	// the desc parameter represents the manifest of targe image.
	Push(ctx context.Context, desc ocispec.Descriptor, ref string) error

	// Image gets the source image object.
	Image() containerd.Image
	// ContentStore gets the content store object of containerd.
	ContentStore() content.Store
	// Client gets the raw containerd client.
	Client() *containerd.Client
}

type LocalProvider struct {
	image        containerd.Image
	client       *containerd.Client
	usePlainHTTP bool
	hosts        remote.HostFunc
}

func NewLocalProvider(
	client *containerd.Client,
	hosts remote.HostFunc,
) (Provider, error) {
	return &LocalProvider{
		client: client,
		hosts:  hosts,
	}, nil
}

func (pvd *LocalProvider) updateLayerDiffID(ctx context.Context, image ocispec.Descriptor) error {
	cs := pvd.ContentStore()

	maniDescs, err := utils.GetManifests(ctx, cs, image)
	if err != nil {
		return errors.Wrap(err, "get manifests")
	}

	for _, desc := range maniDescs {
		bytes, err := content.ReadBlob(ctx, cs, desc)
		if err != nil {
			return errors.Wrap(err, "read manifest")
		}

		var manifest ocispec.Manifest
		if err := json.Unmarshal(bytes, &manifest); err != nil {
			return errors.Wrap(err, "unmarshal manifest")
		}

		diffIDs, err := images.RootFS(ctx, cs, manifest.Config)
		if err != nil {
			return errors.Wrap(err, "get diff ids from config")
		}
		if len(manifest.Layers) != len(diffIDs) {
			return fmt.Errorf("unmatched layers between manifest and config: %d != %d", len(manifest.Layers), len(diffIDs))
		}

		for idx, diffID := range diffIDs {
			layerDesc := manifest.Layers[idx]
			info, err := cs.Info(ctx, layerDesc.Digest)
			if err != nil {
				return errors.Wrap(err, "get layer info")
			}
			if info.Labels == nil {
				info.Labels = map[string]string{}
			}
			info.Labels[labels.LabelUncompressed] = diffID.String()
			_, err = cs.Update(ctx, info)
			if err != nil {
				return errors.Wrap(err, "update layer label")
			}
		}
	}

	return nil
}

func (pvd *LocalProvider) UsePlainHTTP() {
	pvd.usePlainHTTP = true
}

func (pvd *LocalProvider) Resolver(ref string) (remotes.Resolver, error) {
	credFunc, insecure, err := pvd.hosts(ref)
	if err != nil {
		return nil, err
	}
	return remote.NewResolver(insecure, pvd.usePlainHTTP, credFunc), nil
}

func (pvd *LocalProvider) Pull(ctx context.Context, ref string) error {
	resolver, err := pvd.Resolver(ref)
	if err != nil {
		return err
	}

	// TODO: enable configuring the target platforms.
	platformMatcher := nydusUtils.ExcludeNydusPlatformComparer{MatchComparer: platforms.All}

	opts := []containerd.RemoteOpt{
		// TODO: sets max concurrent downloaded layer limit by containerd.WithMaxConcurrentDownloads.
		containerd.WithPlatformMatcher(platformMatcher),
		containerd.WithImageHandler(images.HandlerFunc(
			func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
				if images.IsLayerType(desc.MediaType) {
					logger.Debugf("pulling layer %s", desc.Digest)
				}
				return nil, nil
			},
		)),
		containerd.WithResolver(resolver),
	}

	// Pull the source image from remote registry.
	image, err := pvd.client.Fetch(ctx, ref, opts...)
	if err != nil {
		return errors.Wrap(err, "pull source image")
	}

	// Write a diff id label of layer in content store for simplifying
	// diff id calculation to speed up the conversion.
	// See: https://github.com/containerd/containerd/blob/e4fefea5544d259177abb85b64e428702ac49c97/images/diffid.go#L49
	if err := pvd.updateLayerDiffID(ctx, image.Target); err != nil {
		return errors.Wrap(err, "update layer diff id")
	}

	pvd.image = containerd.NewImageWithPlatform(pvd.client, image, platformMatcher)

	return nil
}

func (pvd *LocalProvider) Push(ctx context.Context, desc ocispec.Descriptor, ref string) error {
	resolver, err := pvd.Resolver(ref)
	if err != nil {
		return err
	}

	// TODO: sets max concurrent uploaded layer limit by containerd.WithMaxConcurrentUploadedLayers.
	return pvd.client.Push(ctx, ref, desc, containerd.WithResolver(resolver))
}

func (pvd *LocalProvider) Image() containerd.Image {
	return pvd.image
}

func (pvd *LocalProvider) ContentStore() content.Store {
	return pvd.client.ContentStore()
}

func (pvd *LocalProvider) Client() *containerd.Client {
	return pvd.client
}
