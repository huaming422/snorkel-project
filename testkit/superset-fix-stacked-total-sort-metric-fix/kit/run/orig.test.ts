/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */
//
// Behavioral checks for apache/superset #42881.
//
// WHAT IS TESTED, AND THROUGH WHICH INTERFACE
// -------------------------------------------
// The symptom: on a stacked time-series bar chart that prints the per-bar
// total ("Only Total"), the total is too large whenever the chart is sorted
// by a metric that is NOT one of the displayed/stacked metrics. That sort-only
// metric's value is present in the row data and leaks into the printed total.
//
// Every check drives the plugin's PUBLIC, STABLE entry point —
// `transformProps(chartProps)` — and reads the total straight off the produced
// ECharts option:
//
//   transformProps(chartProps).echartOptions.series[i]
//       .label.formatter({ value, dataIndex, seriesIndex })
//
// With "Only Total" on, exactly one series per x position returns the total
// string and the rest return '', so filtering out '' yields the per-bar totals.
// This is the same read-out the chart renders and the same shape the sibling
// Timeseries transform tests use.
//
// The checks deliberately read ONLY the formatter output. They never import or
// inspect the internal totals routine, its parameters, the number of arguments
// it receives, or any excluded-keys collection. That is what lets every
// equally-valid approach pass — whether the exclusion is plumbed through the
// totals routine's options, the sort-only columns are stripped from the rows
// before the totals are summed, or the exclusion is expressed with a set, an
// array membership check, a pre-filter, or an early continue, under any
// identifier names.
//
import {
  ChartDataResponseResult,
  DataRecord,
  SqlaFormData,
} from '@superset-ui/core';
import transformProps from '../../src/Timeseries/transformProps';
import {
  EchartsTimeseriesFormData,
} from '../../src/Timeseries/types';
import { EchartsTimeseriesChartProps } from '../../src/types';
import { DEFAULT_FORM_DATA } from '../../src/Timeseries/constants';
import { createEchartsTimeseriesTestChartProps } from '../helpers';
import { createTestData } from './helpers';

// --- Local test-data factories (mirrors of the file-local helpers used by the
// sibling transformProps tests; replicated here so this file is self-contained
// when copied next to them). ----------------------------------------------
function createTestQueryData(
  data: unknown[],
  overrides?: Partial<ChartDataResponseResult> & {
    label_map?: Record<string, string[]>;
  },
): ChartDataResponseResult {
  return {
    annotation_data: null,
    cache_key: null,
    cache_timeout: null,
    cached_dttm: null,
    queried_dttm: null,
    data: data as DataRecord[],
    colnames: [],
    coltypes: [],
    error: null,
    is_cached: false,
    query: '',
    rowcount: data.length,
    sql_rowcount: data.length,
    stacktrace: null,
    status: 'success',
    from_dttm: null,
    to_dttm: null,
    label_map: {},
    ...overrides,
  } as ChartDataResponseResult & { label_map?: Record<string, string[]> };
}

function createTestChartProps(config: {
  formData?: Partial<EchartsTimeseriesFormData>;
  queriesData?: ChartDataResponseResult[];
  datasource?: {
    verboseMap?: Record<string, string>;
    columnFormats?: Record<string, string>;
  };
}): EchartsTimeseriesChartProps {
  return createEchartsTimeseriesTestChartProps<
    EchartsTimeseriesFormData,
    EchartsTimeseriesChartProps
  >({
    defaultFormData: DEFAULT_FORM_DATA,
    defaultVizType: 'my_viz',
    defaultQueriesData: [],
    ...config,
  });
}

// Base form data for a stacked bar chart that prints the per-bar total.
const stackedTotalFormData: SqlaFormData = {
  viz_type: 'my_viz',
  colorScheme: 'bnbColors',
  datasource: '3__table',
  granularity_sqla: 'ds',
  metric: 'sum__num',
  groupby: [],
  showValue: true,
  stack: true,
  onlyTotal: true,
};

// Pull the per-bar total strings off the produced option. Each series returns
// one formatter result per data point; non-total series return '', so the
// remaining non-empty strings are the bar totals in x order.
type LabelFormatter = (params: {
  value: [Date, number];
  dataIndex: number;
  seriesIndex: number;
}) => string;
type TotalSeries = {
  label: { show: boolean; formatter: LabelFormatter };
  data: [Date, number][];
  name: string;
};

function readBarTotals(chartProps: EchartsTimeseriesChartProps): string[] {
  const series = transformProps(chartProps).echartOptions
    .series as TotalSeries[];
  return series
    .flatMap((s, seriesIndex) =>
      s.data.map((value, dataIndex) =>
        s.label.formatter({ value, dataIndex, seriesIndex }),
      ),
    )
    .filter(label => label !== '');
}

describe('stacked total excludes sort-only metric (#42881)', () => {
  // Criterion: stacked_total_excludes_sort_only_metric
  test('a plain-named sort-only metric is left out of the per-bar total', () => {
    // Three displayed metrics sum to 32. The chart is sorted by `sort_metric`,
    // which is not among the displayed metrics, and its value (2) rides along
    // in the row data. The printed total must be the visible sum, 32 — not 34.
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: ['San Francisco', 'New York', 'Boston'],
      timeseries_limit_metric: 'sort_metric',
      x_axis_sort: 'sort_metric',
    };
    const queriesData: ChartDataResponseResult[] = [
      createTestQueryData(
        createTestData(
          [
            {
              'San Francisco': 32,
              'New York': 0,
              Boston: 0,
              sort_metric: 2,
            },
          ],
          { intervalMs: 300000000 },
        ),
      ),
    ];

    const totals = readBarTotals(
      createTestChartProps({ formData, queriesData }),
    );

    expect(totals).toEqual(['32']);
  });

  // Criterion: exclusion_respects_verbose_name
  test('a verbose-named sort-only metric is left out of the per-bar total', () => {
    // When the sort-only metric has a verbose name configured, its column
    // appears in the row data under the verbose label ("Sort By Metric"), not
    // the raw metric label ("sort_metric"). The exclusion must still match it,
    // so the printed total stays 32. An approach that matches only the raw
    // label leaves the value (2) in and prints 34.
    const verboseMap = { sort_metric: 'Sort By Metric' };
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: ['San Francisco', 'New York', 'Boston'],
      timeseries_limit_metric: 'sort_metric',
      x_axis_sort: 'sort_metric',
    };
    const queriesData: ChartDataResponseResult[] = [
      createTestQueryData(
        createTestData(
          [
            {
              'San Francisco': 32,
              'New York': 0,
              Boston: 0,
              'Sort By Metric': 2,
            },
          ],
          { intervalMs: 300000000 },
        ),
      ),
    ];

    const totals = readBarTotals(
      createTestChartProps({
        formData,
        queriesData,
        datasource: { verboseMap },
      }),
    );

    expect(totals).toEqual(['32']);
  });
});

describe('existing stacked-total behavior is preserved (#42881)', () => {
  // Criterion: plain_stacked_total_unchanged
  test('with no sort-only metric the total is the full segment sum', () => {
    // No sort-only metric in play: every numeric column is a displayed segment,
    // so each bar total is the full sum of its segments across the two rows.
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: ['San Francisco', 'New York', 'Boston'],
    };
    const queriesData: ChartDataResponseResult[] = [
      createTestQueryData(
        createTestData(
          [
            { 'San Francisco': 5, 'New York': 8, Boston: 6 },
            { 'San Francisco': 2, 'New York': 7, Boston: 2 },
          ],
          { intervalMs: 300000000 },
        ),
      ),
    ];

    const totals = readBarTotals(
      createTestChartProps({ formData, queriesData }),
    );

    expect(totals).toEqual(['19', '11']);
  });

  // Criterion: legend_toggle_still_excluded
  test('a legend-toggled-off series is still left out of the total', () => {
    // Pre-existing behavior unrelated to the sort-only metric: a series the
    // viewer has toggled off in the legend must not count toward the total.
    // Toggling "New York" off leaves 5 + 6 = 11 and 2 + 2 = 4.
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: ['San Francisco', 'New York', 'Boston'],
    };
    const queriesData: ChartDataResponseResult[] = [
      createTestQueryData(
        createTestData(
          [
            { 'San Francisco': 5, 'New York': 8, Boston: 6 },
            { 'San Francisco': 2, 'New York': 7, Boston: 2 },
          ],
          { intervalMs: 300000000 },
        ),
      ),
    ];

    const chartProps = {
      ...createTestChartProps({ formData, queriesData }),
      legendState: { 'San Francisco': true, 'New York': false, Boston: true },
    } as EchartsTimeseriesChartProps;

    const totals = readBarTotals(chartProps);

    expect(totals).toEqual(['11', '4']);
  });
});
