// The browser's errors and fetch spans (task 6.9, docs/observability.md section 1), loaded with
// the metrics after the page has loaded (telemetry.ts). They go to the collector's browser
// receiver through the gateway's /otlp/v1/traces, which keeps only these span names and
// attributes (infra/observability/otelcol/browser.yaml):
//   fetch <METHOD> <route>   one per API call; the route has ids and the query string removed
//   error                    an uncaught error or unhandled rejection, message cut to 200 characters
// No trace context goes to the API: no global propagator is registered, so fetches carry no
// traceparent. Anyone can post to the telemetry endpoint, and a client must not choose the
// backend's trace ids or sampling. Browser spans are their own traces.
import { SpanStatusCode, type Span, type Tracer } from '@opentelemetry/api'
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http'
import { registerInstrumentations } from '@opentelemetry/instrumentation'
import { FetchInstrumentation } from '@opentelemetry/instrumentation-fetch'
import { resourceFromAttributes } from '@opentelemetry/resources'
import { BatchSpanProcessor, type SpanExporter } from '@opentelemetry/sdk-trace-base'
import { WebTracerProvider } from '@opentelemetry/sdk-trace-web'

export const MAX_MESSAGE = 200

/** A URL's path as a route: segments that look like ids (digits, long tokens, prefixed ids such
 * as u_… or m_…) become {id}, and the query string is gone. */
export function routeOf(url: string): string {
  let path: string
  try {
    path = new URL(url, window.location.origin).pathname
  } catch {
    return '/unknown'
  }
  return path
    .split('/')
    .map((seg) => (/\d/.test(seg) || seg.length >= 20 || /^[a-z]{1,4}_/.test(seg) ? '{id}' : seg))
    .join('/')
}

/** Renames a fetch span to `fetch <METHOD> <route>` and keeps only safe attributes. */
export function describeFetch(span: Span, request: Request | RequestInit, result: Response | unknown) {
  const method = (request.method ?? 'GET').toUpperCase()
  if (result instanceof Response && result.url) {
    const route = routeOf(result.url)
    span.updateName(`fetch ${method} ${route}`)
    span.setAttribute('http.route', route)
    span.setAttribute('http.response.status_code', result.status)
  } else {
    span.updateName(`fetch ${method} failed`)
  }
  span.setAttribute('http.request.method', method)
}

/** Records uncaught errors and unhandled rejections as `error` spans on tracer. */
export function recordErrors(tracer: Tracer, page: () => string, target: Window = window): void {
  const record = (type: string, message: string) => {
    const span = tracer.startSpan('error', {
      attributes: { 'exception.type': type, 'exception.message': message.slice(0, MAX_MESSAGE), page: page() },
    })
    span.setStatus({ code: SpanStatusCode.ERROR })
    span.end()
  }
  target.addEventListener('error', (e: ErrorEvent) => {
    record(e.error instanceof Error ? e.error.name : 'Error', e.message || String(e.error))
  })
  target.addEventListener('unhandledrejection', (e: PromiseRejectionEvent) => {
    const r = e.reason as unknown
    record(r instanceof Error ? r.name : 'UnhandledRejection', r instanceof Error ? r.message : String(r))
  })
}

/** A tracer provider exporting in batches to exporter, with the same resource as the metrics. */
export function tracerProvider(exporter: SpanExporter): WebTracerProvider {
  return new WebTracerProvider({
    resource: resourceFromAttributes({
      'service.name': 'web',
      'deployment.environment': window.location.hostname === 'localhost' ? 'local' : 'aws',
    }),
    spanProcessors: [new BatchSpanProcessor(exporter, { scheduledDelayMillis: 5_000, maxExportBatchSize: 50 })],
  })
}

export function startTracing(page: () => string): void {
  const provider = tracerProvider(new OTLPTraceExporter({ url: '/otlp/v1/traces' }))
  // Not provider.register(): that would also install the W3C propagator, and fetches would carry
  // a traceparent to the API.
  registerInstrumentations({
    tracerProvider: provider,
    instrumentations: [
      new FetchInstrumentation({
        ignoreUrls: [/\/otlp\//],
        propagateTraceHeaderCorsUrls: [],
        applyCustomAttributesOnSpan: describeFetch,
      }),
    ],
  })
  recordErrors(provider.getTracer('web'), page)
  window.addEventListener('pagehide', () => void provider.forceFlush().catch(() => {}))
}
