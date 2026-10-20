import { describe, expect, it } from 'vitest'
import { AggregationTemporality, InMemoryMetricExporter, type HistogramMetricData } from '@opentelemetry/sdk-metrics'
import { instruments, provider } from './telemetry.ts'

// The collector's browser receiver accepts exactly these bounds (infra/observability/otelcol/
// browser.yaml); a change here must change it there.
const BOUNDS: Record<string, number[]> = {
  'web.vitals.lcp': [500, 1000, 1500, 2000, 2500, 3000, 4000, 6000, 10000],
  'web.vitals.inp': [25, 50, 100, 150, 200, 300, 500, 1000, 2000],
  'web.vitals.cls': [0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1],
  'web.ws.rtt': [5, 10, 25, 50, 100, 250, 500, 1000, 2500],
}

describe('telemetry', () => {
  it('records the vitals by page and the round trip, as deltas with the agreed bounds', async () => {
    const exporter = new InMemoryMetricExporter(AggregationTemporality.DELTA)
    const meterProvider = provider(exporter)
    const rec = instruments(meterProvider.getMeter('web'), () => 'room')
    rec.vital({ name: 'LCP', value: 1800 })
    rec.vital({ name: 'INP', value: 120 })
    rec.vital({ name: 'CLS', value: 0.04 })
    rec.vital({ name: 'TTFB', value: 300 }) // not one of ours: ignored
    rec.rtt(42)
    await meterProvider.forceFlush()

    const metrics = exporter.getMetrics().flatMap((rm) => rm.scopeMetrics.flatMap((sm) => sm.metrics))
    const byName = Object.fromEntries(metrics.map((m) => [m.descriptor.name, m as HistogramMetricData]))
    expect(Object.keys(byName).sort()).toEqual(Object.keys(BOUNDS).sort())
    for (const [name, bounds] of Object.entries(BOUNDS)) {
      const m = byName[name]
      expect(m.aggregationTemporality).toBe(AggregationTemporality.DELTA)
      expect(m.dataPoints).toHaveLength(1)
      expect(m.dataPoints[0].value.buckets.boundaries).toEqual(bounds)
    }
    expect(byName['web.vitals.lcp'].dataPoints[0].attributes).toEqual({ page: 'room' })
    expect(byName['web.vitals.lcp'].descriptor.unit).toBe('ms')
    expect(byName['web.vitals.cls'].descriptor.unit).toBe('')
    expect(byName['web.ws.rtt'].dataPoints[0].attributes).toEqual({}) // no page: the room's only
    expect(byName['web.vitals.lcp'].dataPoints[0].value.sum).toBe(1800)
    await meterProvider.shutdown()
  })
})
