package vertica_prometheus_exporter

// (c) Copyright [2018-2022] Micro Focus or one of its affiliates.
// Licensed under the Apache License, Version 2.0 (the "License");
// You may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// 
// http://www.apache.org/licenses/LICENSE-2.0
// 
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// 
// MIT license brought forward from the sql-exporter repo by burningalchemist
// 
// MIT License
// 
// Copyright (c) 2017 Alin Sinpalean
// 
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
// 
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
// 
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vertica/vertica-prometheus-exporter/config"
	"google.golang.org/protobuf/proto"
)

const envDsnOverride = "SQLEXPORTER_TARGET_DSN"

var dsnOverride = flag.String("config.data-source-name", "", "Data source name to override the value in the configuration file with.")

// Exporter is a prometheus.Gatherer that gathers SQL metrics from targets and merges them with the default registry.
type Exporter interface {
	prometheus.Gatherer

	// WithContext returns a (single use) copy of the Exporter, which will use the provided context for Gather() calls.
	WithContext(context.Context) Exporter
	// Config returns the Exporter's underlying Config object.
	Config() *config.Config
	UpdateTarget([]Target)
}

type exporter struct {
	config  *config.Config
	targets []Target

	ctx context.Context
}

// NewExporter returns a new Exporter with the provided config.
func NewExporter(configFile string) (Exporter, error) {
	c, err := config.Load(configFile)
	if err != nil {
		return nil, err
	}

	if val, ok := os.LookupEnv(envDsnOverride); ok {
		*dsnOverride = val
	}
	// Override the DSN if requested (and in single target mode).
	if *dsnOverride != "" {
		if len(c.Jobs) > 0 {
			return nil, fmt.Errorf("the config.data-source-name flag (value %q) only applies in single target mode", *dsnOverride)
		}
		c.Target.DSN = config.Secret(*dsnOverride)
	}

	var targets []Target
	if c.Target != nil {
		target, err := NewTarget("", "", string(c.Target.DSN), c.Target.Collectors(), nil, c.Globals)
		println(target)
		if err != nil {
			return nil, err
		}
		targets = []Target{target}
	} else {
		if len(c.Jobs) > (config.MaxInt32 / 3) {
			return nil, errors.New("'jobs' list is too large")
		}
		targets = make([]Target, 0, len(c.Jobs)*3)
		for _, jc := range c.Jobs {
			job, err := NewJob(jc, c.Globals)
			if err != nil {
				return nil, err
			}
			targets = append(targets, job.Targets()...)
		}
	}

	return &exporter{
		config:  c,
		targets: targets,
		ctx:     context.Background(),
	}, nil

}

func (e *exporter) WithContext(ctx context.Context) Exporter {
	return &exporter{
		config:  e.config,
		targets: e.targets,
		ctx:     ctx,
	}
}

// Gather implements prometheus.Gatherer.
func (e *exporter) Gather() ([]*dto.MetricFamily, error) {
	var (
		metricChan = make(chan Metric, capMetricChan)
		errs       prometheus.MultiError
	)

	var wg sync.WaitGroup
	wg.Add(len(e.targets))
	for _, t := range e.targets {
		go func(target Target) {
			defer wg.Done()
			target.Collect(e.ctx, metricChan)
		}(t)
	}

	// Wait for all collectors to complete, then close the channel.
	go func() {
		wg.Wait()
		close(metricChan)
	}()

	// Drain metricChan in case of premature return.
	defer func() {
		for range metricChan {
		}
	}()

	// Gather.
	dtoMetricFamilies := make(map[string]*dto.MetricFamily, 10)
	for metric := range metricChan {
		dtoMetric := &dto.Metric{}
		if err := metric.Write(dtoMetric); err != nil {
			errs = append(errs, err)
			continue
		}
		metricDesc := metric.Desc()
		dtoMetricFamily, ok := dtoMetricFamilies[metricDesc.Name()]
		if !ok {
			dtoMetricFamily = &dto.MetricFamily{}
			dtoMetricFamily.Name = proto.String(metricDesc.Name())
			dtoMetricFamily.Help = proto.String(metricDesc.Help())
			switch {
			case dtoMetric.Gauge != nil:
				dtoMetricFamily.Type = dto.MetricType_GAUGE.Enum()
			case dtoMetric.Counter != nil:
				dtoMetricFamily.Type = dto.MetricType_COUNTER.Enum()
			default:
				errs = append(errs, fmt.Errorf("don't know how to handle metric %v", dtoMetric))
				continue
			}
			dtoMetricFamilies[metricDesc.Name()] = dtoMetricFamily
		}
		dtoMetricFamily.Metric = append(dtoMetricFamily.Metric, dtoMetric)
	}

	// No need to sort metric families, prometheus.Gatherers will do that for us when merging.
	result := make([]*dto.MetricFamily, 0, len(dtoMetricFamilies))
	for _, mf := range dtoMetricFamilies {
		result = append(result, mf)
	}
	return result, errs
}

// Config implements Exporter.
func (e *exporter) Config() *config.Config {
	return e.config
}

func (e *exporter) UpdateTarget(target []Target) {
	e.targets = target
}
