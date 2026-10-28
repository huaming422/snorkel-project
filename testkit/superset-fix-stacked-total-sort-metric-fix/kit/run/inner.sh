#!/bin/bash
# usage: inner.sh <patch> <testfile>
cd /repo/superset
if [ -s "$1" ]; then git apply "$1" || { echo APPLY_FAIL; exit 3; }; fi
cp "$2" superset-frontend/plugins/plugin-chart-echarts/test/Timeseries/verify_stacked_total_sort_metric.test.ts
mkdir -p superset-frontend/node_modules/ace-builds/src-min-noconflict
echo 'module.exports = {};' > superset-frontend/node_modules/ace-builds/src-min-noconflict/mode-handlebars.js
cd superset-frontend
npx jest --testPathPatterns plugins/plugin-chart-echarts/test/Timeseries/verify_stacked_total_sort_metric.test.ts 2>&1 | grep -E "✓|✕|Tests:|●.*›" | sort -u
