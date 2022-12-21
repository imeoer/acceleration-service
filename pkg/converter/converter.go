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

package converter

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/defaults"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/goharbor/acceleration-service/pkg/content"
	"github.com/goharbor/acceleration-service/pkg/driver"
	"github.com/goharbor/acceleration-service/pkg/errdefs"
)

var logger = logrus.WithField("module", "converter")

type LocalConverter struct {
	client   *containerd.Client
	driver   driver.Driver
	provider content.Provider
	opts     ConvertOpts
}

func NewLocalConverter(opts ...ConvertOpt) (*LocalConverter, error) {
	var options ConvertOpts
	for _, opt := range opts {
		if err := opt(&options); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	if options.client == nil {
		client, err := containerd.New(defaults.DefaultAddress)
		if err != nil {
			return nil, errors.Wrapf(err, "connect to containerd address %s", defaults.DefaultAddress)
		}
		options.client = client
	}

	provider, err := content.NewLocalProvider(
		options.client,
		options.hosts,
	)
	if err != nil {
		return nil, errors.Wrap(err, "create content provider")
	}

	driver, err := driver.NewLocalDriver(options.driverType, options.driverConfig)
	if err != nil {
		return nil, errors.Wrap(err, "create driver")
	}

	handler := &LocalConverter{
		client:   options.client,
		driver:   driver,
		provider: provider,
		opts:     options,
	}

	return handler, nil
}

func (cvt *LocalConverter) Convert(ctx context.Context, source, target string) error {
	ctx, done, err := cvt.client.WithLease(ctx)
	if err != nil {
		return errors.Wrap(err, "create lease")
	}
	defer done(ctx)

	logger.Infof("pulling image %s", source)
	start := time.Now()
	if err := cvt.provider.Pull(ctx, source); err != nil {
		if errdefs.NeedsRetryWithHTTP(err) {
			logger.Infof("try to pull with plain HTTP for %s", source)
			cvt.provider.UsePlainHTTP()
			if err := cvt.provider.Pull(ctx, source); err != nil {
				return errors.Wrap(err, "try to pull image")
			}
		} else {
			return errors.Wrap(err, "pull image")
		}
	}
	logger.Infof("pulled image %s, elapse %s", source, time.Since(start))

	logger.Infof("converting image %s", source)
	start = time.Now()
	desc, err := cvt.driver.Convert(ctx, cvt.provider)
	if err != nil {
		return errors.Wrap(err, "convert image")
	}
	logger.Infof("converted image %s, elapse %s", target, time.Since(start))

	start = time.Now()
	logger.Infof("pushing image %s", target)
	if err := cvt.provider.Push(ctx, *desc, target); err != nil {
		if errdefs.NeedsRetryWithHTTP(err) {
			logger.Infof("try to push with plain HTTP for %s", target)
			cvt.provider.UsePlainHTTP()
			if err := cvt.provider.Push(ctx, *desc, target); err != nil {
				return errors.Wrap(err, "try to push image")
			}
		} else {
			return errors.Wrap(err, "push image")
		}
	}
	logger.Infof("pushed image %s, elapse %s", target, time.Since(start))

	return nil
}
