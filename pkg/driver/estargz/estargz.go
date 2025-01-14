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

package estargz

import (
	"context"
	"strconv"

	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	"github.com/goharbor/acceleration-service/pkg/content"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type Driver struct {
	cfg map[string]string
}

func New(cfg map[string]string) (*Driver, error) {
	return &Driver{cfg}, nil
}

func (d *Driver) Convert(ctx context.Context, p content.Provider) (*ocispec.Descriptor, error) {
	opts, docker2oci, err := getESGZConvertOpts(d.cfg)
	if err != nil {
		return nil, errors.Wrap(err, "parse estargz conversion options")
	}
	platformMC := platforms.All // TODO: enable to configure the target platforms
	return converter.DefaultIndexConvertFunc(estargzconvert.LayerConvertFunc(opts...), docker2oci, platformMC)(
		ctx, p.ContentStore(), p.Image().Target())
}

func getESGZConvertOpts(cfg map[string]string) (opts []estargz.Option, docker2oci bool, err error) {
	if s, ok := cfg["docker2oci"]; ok {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, false, err
		}
		docker2oci = b
	}
	return
}
