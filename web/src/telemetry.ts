// The OpenTelemetry side of real-user monitoring (rum.ts loads this after the page has loaded).
//
// Metrics (docs/observability.md, section 3):
//   web.vitals.lcp   Histogram (ms)  Largest Contentful Paint, attribute page
//   web.vitals.inp   Histogram (ms)  Interaction to Next Paint, attribute page
//   web.vitals.cls   Histogram       Cumulative Layout Shift (unitless), attribute page
//   web.ws.rtt       Histogram (ms)  the room's WebSocket ping round trip
//
// Each page load is a short-lived process, so it sends deltas: the collector's browser receiver
// adds every browser's deltas into one cumulative series per page (infra/observability/otelcol/
// browser.yaml). The vitals arrive when the page is hidden (web-vitals reports them then), so
// the reader flushes on that too; the browser exporter sends with sendBeacon, which outlives the
// page.
import { type Meter } from '@opentelemetry/api'
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-http'
import { resourceFromAttributes } from '@opentelemetry/resources'
import {
  AggregationTemporality,
  MeterProvider,
  PeriodicExportingMetricReader,
  type PushMetricExporter,
} from '@opentelemetry/sdk-metrics'
import { onCLS, onINP, onLCP, type Metric } from 'web-vitals'
import { attach, pageName } from './rum.ts'

// Bucket bounds around Google's thresholds (good / poor): LCP 2.5 s / 4 s, INP 200 / 500 ms,
// CLS 0.1 / 0.25.
const LCP_BOUNDS = [500, 1000, 1500, 2000, 2500, 3000, 4000, 6000, 10000]
const INP_BOUNDS = [25, 50, 100, 150, 200, 300, 500, 1000, 2000]
const CLS_BOUNDS = [0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1]
const RTT_BOUNDS = [5, 10, 25, 50, 100, 250, 500, 1000, 2500]

/** The instruments, on any meter (tests use an in-memory one). */
export function instruments(meter: Meter, page: () => string) {
  const histogram = (name: string, unit: string, description: string, bounds: number[]) =>
    meter.createHistogram(name, { unit, description, advice: { explicitBucketBoundaries: bounds } })
  const lcp = histogram('web.vitals.lcp', 'ms', 'Largest Contentful Paint', LCP_BOUNDS)
  const inp = histogram('web.vitals.inp', 'ms', 'Interaction to Next Paint', INP_BOUNDS)
  const cls = histogram('web.vitals.cls', '', 'Cumulative Layout Shift', CLS_BOUNDS)
  const rtt = histogram('web.ws.rtt', 'ms', "The room's WebSocket ping round trip", RTT_BOUNDS)
  return {
    vital(metric: Pick<Metric, 'name' | 'value'>) {
      const target = metric.name === 'LCP' ? lcp : metric.name === 'INP' ? inp : metric.name === 'CLS' ? cls : null
      target?.record(metric.value, { page: page() })
    },
    rtt(ms: number) {
      rtt.record(ms)
    },
  }
}

/** A delta-temporality reader around exporter, flushed every minute and when the page hides. */
export function provider(exporter: PushMetricExporter): MeterProvider {
  return new MeterProvider({
    resource: resourceFromAttributes({
      'service.name': 'web',
      'deployment.environment': window.location.hostname === 'localhost' ? 'local' : 'aws',
    }),
    readers: [new PeriodicExportingMetricReader({ exporter, exportIntervalMillis: 60_000 })],
  })
}

export function start(): void {
  const exporter = new OTLPMetricExporter({
    url: '/otlp/v1/metrics',
    temporalityPreference: AggregationTemporality.DELTA,
  })
  const meterProvider = provider(exporter)
  const recorder = instruments(meterProvider.getMeter('web'), () => pageName(window.location.pathname))
  onLCP(recorder.vital)
  onINP(recorder.vital)
  onCLS(recorder.vital)
  attach(recorder)
  // Registered after web-vitals' own listeners, so the final values are recorded before the flush.
  const flush = () => {
    if (document.visibilityState === 'hidden') void meterProvider.forceFlush().catch(() => {})
  }
  document.addEventListener('visibilitychange', flush)
  window.addEventListener('pagehide', () => void meterProvider.forceFlush().catch(() => {}))
}
