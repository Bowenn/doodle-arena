import { context, propagation, trace } from '@opentelemetry/api'
import { InMemorySpanExporter, SimpleSpanProcessor } from '@opentelemetry/sdk-trace-base'
import { WebTracerProvider } from '@opentelemetry/sdk-trace-web'
import { describe, expect, it } from 'vitest'
import { MAX_MESSAGE, describeFetch, recordErrors, routeOf, startTracing } from './tracing.ts'

describe('routeOf', () => {
  it('turns ids into {id} and drops the query string', () => {
    expect(routeOf('/api/assets/drawings/01a16b14-2046-710f-aa46-b03eafc091c6/replay')).toBe('/api/assets/drawings/{id}/replay')
    expect(routeOf('http://localhost:8080/api/assets/drawings?ids=a,b&limit=20')).toBe('/api/assets/drawings')
    expect(routeOf('/api/market/drawings/123')).toBe('/api/market/drawings/{id}')
    expect(routeOf('/api/lobby/rooms/k3f9q2/join')).toBe('/api/lobby/rooms/{id}/join')
    expect(routeOf('/api/market/wallet')).toBe('/api/market/wallet')
    expect(routeOf('/api/auth/.well-known/jwks.json')).toBe('/api/auth/.well-known/jwks.json')
    expect(routeOf('/api/notifier/endpoints/u_0123/test')).toBe('/api/notifier/endpoints/{id}/test')
  })
})

function fakeSpan() {
  const span = { name: '', attributes: {} as Record<string, unknown> }
  return {
    span,
    api: {
      updateName: (n: string) => (span.name = n),
      setAttribute: (k: string, v: unknown) => (span.attributes[k] = v),
    } as never,
  }
}

describe('describeFetch', () => {
  it('names a fetch by method and route, without its URL', () => {
    const { span, api } = fakeSpan()
    const response = new Response(null, { status: 404 })
    Object.defineProperty(response, 'url', { value: 'http://localhost:8080/api/lobby/rooms/k3f9q2/join?x=1' })
    describeFetch(api, { method: 'post' }, response)
    expect(span).toEqual({
      name: 'fetch POST /api/lobby/rooms/{id}/join',
      attributes: { 'http.route': '/api/lobby/rooms/{id}/join', 'http.response.status_code': 404, 'http.request.method': 'POST' },
    })
  })

  it('names a failed fetch without a URL', () => {
    const { span, api } = fakeSpan()
    describeFetch(api, {}, new TypeError('Failed to fetch'))
    expect(span.name).toBe('fetch GET failed')
  })
})

describe('recordErrors', () => {
  it('records uncaught errors and rejections as error spans with a short message', async () => {
    const exporter = new InMemorySpanExporter()
    const provider = new WebTracerProvider({ spanProcessors: [new SimpleSpanProcessor(exporter)] })
    const target = new EventTarget() as Window
    recordErrors(provider.getTracer('t'), () => 'room', target)

    target.dispatchEvent(new ErrorEvent('error', { message: 'x'.repeat(500), error: new RangeError('boom') }))
    const rejection = new Event('unhandledrejection') as PromiseRejectionEvent
    Object.defineProperty(rejection, 'reason', { value: new Error('nope') })
    target.dispatchEvent(rejection)

    const spans = exporter.getFinishedSpans()
    expect(spans.map((s) => [s.name, s.attributes['exception.type'], s.attributes.page, s.status.code])).toEqual([
      ['error', 'RangeError', 'room', 2],
      ['error', 'Error', 'room', 2],
    ])
    expect(String(spans[0].attributes['exception.message'])).toHaveLength(MAX_MESSAGE)
    expect(spans[1].attributes['exception.message']).toBe('nope')
  })
})

describe('startTracing', () => {
  it('installs no propagator: fetches to the API carry no traceparent', () => {
    startTracing(() => 'lobby')
    const headers: Record<string, string> = {}
    const span = trace.getTracer('t').startSpan('x')
    propagation.inject(trace.setSpan(context.active(), span), headers)
    expect(propagation.fields()).toEqual([])
    expect(headers).toEqual({})
  })
})
