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
// When the sort-only metric has a verbose name, its column is renamed in the
// row data and it is also drawn as an extra stacked segment.
//
// Every check drives the plugin's public entry point,
// `transformProps(chartProps)`, and reads the result straight off the produced
// ECharts option:
//
//   transformProps(chartProps).echartOptions.series[i]
//       .label.formatter({ value, dataIndex, seriesIndex })
//   transformProps(chartProps).echartOptions.series[i].name
//
// With "Only Total" on, exactly one series per x position returns the total
// string and the rest return '', so filtering out '' yields the per-bar totals.
// The series names are the stacked segments the chart draws. This is the same
// read-out the chart renders and the same shape the sibling Timeseries
// transform tests use.
//
// The checks read only the rendered series and their label output. They do
// not import or inspect the internal totals routine, its parameters, or any
// excluded-keys collection, so the contract is the same whether the exclusion
// goes through the totals routine's options, the sort-only columns are
// stripped from the rows before summing, or the membership test uses a set,
// an array, a pre-filter or an early continue.
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

const displayedMetrics = ['San Francisco', 'New York', 'Boston'];

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

function renderedSeries(
  chartProps: EchartsTimeseriesChartProps,
): TotalSeries[] {
  return transformProps(chartProps).echartOptions.series as TotalSeries[];
}

function readBarTotals(chartProps: EchartsTimeseriesChartProps): string[] {
  return renderedSeries(chartProps)
    .flatMap((s, seriesIndex) =>
      s.data.map((value, dataIndex) =>
        s.label.formatter({ value, dataIndex, seriesIndex }),
      ),
    )
    .filter(label => label !== '');
}

// Names of the stacked segments the chart draws, order-independent.
function readSeriesNames(chartProps: EchartsTimeseriesChartProps): string[] {
  return renderedSeries(chartProps)
    .map(s => s.name)
    .sort();
}

describe('stacked total excludes sort-only metric (#42881)', () => {
  test('a plain-named sort-only metric is left out of the per-bar total', () => {
    // Three displayed metrics sum to 32. The chart is sorted by `sort_metric`,
    // which is not among the displayed metrics, and its value (2) rides along
    // in the row data. The printed total must be the visible sum, 32, not 34,
    // and the sort metric is not drawn as a segment.
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: displayedMetrics,
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
    const chartProps = createTestChartProps({ formData, queriesData });

    expect(readBarTotals(chartProps)).toEqual(['32']);
    expect(readSeriesNames(chartProps)).toEqual([...displayedMetrics].sort());
  });

  test('a verbose-named sort-only metric is left out of the bars and the total', () => {
    // The query returns the sort-only metric under its raw label
    // ("bucket_order"). The dataset gives it a verbose name ("Bucket Order"),
    // and the chart renames row columns through that verbose map before it
    // builds totals and series. The sort metric must still be excluded: not
    // drawn as a segment, and not counted, so the printed total stays 32.
    // Matching only the raw label leaves the value (2) in and prints 34.
    const verboseMap = { bucket_order: 'Bucket Order' };
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: displayedMetrics,
      timeseries_limit_metric: 'bucket_order',
      x_axis_sort: 'bucket_order',
    };
    const queriesData: ChartDataResponseResult[] = [
      createTestQueryData(
        createTestData(
          [
            {
              'San Francisco': 32,
              'New York': 0,
              Boston: 0,
              bucket_order: 2,
            },
          ],
          { intervalMs: 300000000 },
        ),
      ),
    ];
    const chartProps = createTestChartProps({
      formData,
      queriesData,
      datasource: { verboseMap },
    });

    expect(readBarTotals(chartProps)).toEqual(['32']);
    expect(readSeriesNames(chartProps)).toEqual([...displayedMetrics].sort());
  });
});

describe('existing stacked-total behavior is preserved (#42881)', () => {
  test('with no sort-only metric the total is the full segment sum', () => {
    // No sort-only metric in play: every numeric column is a displayed segment,
    // so each bar total is the full sum of its segments across the two rows.
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: displayedMetrics,
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

  test('sorting by a displayed metric keeps that metric in the total', () => {
    // The sort metric here is one of the plotted metrics, so it is a real
    // segment and must stay in the total: 5 + 8 + 6 = 19 and 2 + 7 + 2 = 11.
    // Totals are compared as a set because the x-axis sort may reorder bars.
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: displayedMetrics,
      timeseries_limit_metric: 'New York',
      x_axis_sort: 'New York',
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
    const chartProps = createTestChartProps({ formData, queriesData });

    expect([...readBarTotals(chartProps)].sort()).toEqual(['11', '19']);
    expect(readSeriesNames(chartProps)).toEqual([...displayedMetrics].sort());
  });

  test('a legend-toggled-off series is still left out of the total', () => {
    // Pre-existing behavior unrelated to the sort-only metric: a series the
    // viewer has toggled off in the legend must not count toward the total.
    // Toggling "New York" off leaves 5 + 6 = 11 and 2 + 2 = 4.
    const formData: SqlaFormData = {
      ...stackedTotalFormData,
      metrics: displayedMetrics,
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
