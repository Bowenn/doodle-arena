// Developer settings: API keys for scripts, and webhook endpoints with their delivery log.
// Keys and signing secrets are shown once, right after they're created, and never again.

import { useEffect, useState } from 'react'
import { ApiError } from '../api.ts'
import {
  createApiKey,
  createEndpoint,
  deleteEndpoint,
  EVENT_TYPES,
  listApiKeys,
  listDeliveries,
  listEndpoints,
  revokeApiKey,
  sendTest,
  type ApiKey,
  type Delivery,
  type Endpoint,
} from '../developer.ts'

function message(e: unknown, fallback: string): string {
  return e instanceof ApiError ? e.message : fallback
}

export function Developer() {
  return (
    <main>
      <h1>Developer settings</h1>
      <p className="muted">
        Receive your own sales, purchases, royalties and coin credits as signed webhooks. Manage endpoints here, or
        from a script with an API key.
      </p>
      <ApiKeys />
      <Webhooks />
    </main>
  )
}

/** A key or secret shown once, with a copy button. */
function ShownOnce({ label, value, onDone }: { label: string; value: string; onDone: () => void }) {
  const [copied, setCopied] = useState(false)
  return (
    <div role="status">
      <p>
        {label} This is the only time it is shown: copy it now.
      </p>
      <p className="actions">
        <code className="secret">{value}</code>
        <button
          type="button"
          onClick={() => {
            navigator.clipboard
              ?.writeText(value)
              .then(() => setCopied(true))
              .catch(() => {})
          }}
        >
          {copied ? 'Copied' : 'Copy'}
        </button>
        <button type="button" onClick={onDone}>
          Done
        </button>
      </p>
    </div>
  )
}

function ApiKeys() {
  const [keys, setKeys] = useState<ApiKey[] | null>(null)
  const [name, setName] = useState('')
  const [created, setCreated] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    listApiKeys()
      .then(setKeys)
      .catch((e: unknown) => setError(message(e, 'Could not load your API keys.')))
  }, [])

  function create() {
    setError(null)
    createApiKey(name.trim())
      .then((k) => {
        setCreated(k.key)
        setName('')
        const { key: _secret, ...shown } = k
        setKeys((prev) => [shown, ...(prev ?? [])])
      })
      .catch((e: unknown) => setError(message(e, 'Could not create the key.')))
  }

  function revoke(id: string) {
    setError(null)
    revokeApiKey(id)
      .then(() => setKeys((prev) => prev?.filter((k) => k.id !== id) ?? null))
      .catch((e: unknown) => setError(message(e, 'Could not revoke the key.')))
  }

  return (
    <section>
      <h2>API keys</h2>
      {created && <ShownOnce label="Your new API key." value={created} onDone={() => setCreated(null)} />}
      {error && <p role="alert">{error}</p>}
      <form
        className="actions"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim()) create()
        }}
      >
        <input aria-label="Key name" placeholder="Key name" maxLength={100} value={name} onChange={(e) => setName(e.target.value)} />
        <button type="submit" disabled={!name.trim()}>
          Create key
        </button>
      </form>
      {keys && keys.length === 0 && <p>No API keys.</p>}
      {keys && keys.length > 0 && (
        <ul className="plain-list">
          {keys.map((k) => (
            <li key={k.id}>
              <strong>{k.name}</strong>
              <code>da_{k.prefix}_…</code>
              <span className="muted">
                {k.last_used_at ? `last used ${new Date(k.last_used_at).toLocaleString()}` : 'never used'}
              </span>
              <button type="button" onClick={() => revoke(k.id)}>
                Revoke
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function Webhooks() {
  const [endpoints, setEndpoints] = useState<Endpoint[] | null>(null)
  const [url, setUrl] = useState('')
  const [types, setTypes] = useState<string[]>([...EVENT_TYPES])
  const [created, setCreated] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    listEndpoints()
      .then(setEndpoints)
      .catch((e: unknown) => setError(message(e, 'Could not load your webhook endpoints.')))
  }, [])

  function register() {
    setError(null)
    createEndpoint(url.trim(), types)
      .then((ep) => {
        setCreated(ep.secret)
        setUrl('')
        const { secret: _secret, ...shown } = ep
        setEndpoints((prev) => [shown, ...(prev ?? [])])
      })
      .catch((e: unknown) => setError(message(e, 'Could not register the endpoint.')))
  }

  return (
    <section>
      <h2>Webhooks</h2>
      {created && <ShownOnce label="The endpoint's signing secret." value={created} onDone={() => setCreated(null)} />}
      {error && <p role="alert">{error}</p>}
      <form
        className="actions"
        onSubmit={(e) => {
          e.preventDefault()
          if (url.trim() && types.length > 0) register()
        }}
      >
        <input
          aria-label="Endpoint URL"
          placeholder="https://example.com/hooks/doodle"
          size={40}
          value={url}
          onChange={(e) => setUrl(e.target.value)}
        />
        {EVENT_TYPES.map((t) => (
          <label key={t}>
            <input
              type="checkbox"
              checked={types.includes(t)}
              onChange={(e) => setTypes((prev) => (e.target.checked ? [...prev, t] : prev.filter((x) => x !== t)))}
            />{' '}
            {t}
          </label>
        ))}
        <button type="submit" disabled={!url.trim() || types.length === 0}>
          Register endpoint
        </button>
      </form>
      {endpoints && endpoints.length === 0 && <p>No webhook endpoints.</p>}
      {endpoints && endpoints.length > 0 && (
        <ul className="plain-list">
          {endpoints.map((ep) => (
            <EndpointRow
              key={ep.id}
              endpoint={ep}
              onDeleted={() => setEndpoints((prev) => prev?.filter((x) => x.id !== ep.id) ?? null)}
            />
          ))}
        </ul>
      )}
    </section>
  )
}

function EndpointRow({ endpoint, onDeleted }: { endpoint: Endpoint; onDeleted: () => void }) {
  const [deliveries, setDeliveries] = useState<Delivery[] | null>(null)
  const [note, setNote] = useState<string | null>(null)

  function load() {
    listDeliveries(endpoint.id)
      .then(setDeliveries)
      .catch((e: unknown) => setNote(message(e, 'Could not load the deliveries.')))
  }

  function test() {
    setNote(null)
    sendTest(endpoint.id)
      .then(() => {
        setNote('Test event queued.')
        load()
      })
      .catch((e: unknown) => setNote(message(e, 'Could not send a test event.')))
  }

  function remove() {
    deleteEndpoint(endpoint.id)
      .then(onDeleted)
      .catch((e: unknown) => setNote(message(e, 'Could not delete the endpoint.')))
  }

  return (
    <li className="endpoint">
      <code>{endpoint.url}</code>
      <span className="muted">{endpoint.event_types.join(', ')}</span>
      <span className="actions">
        <button type="button" onClick={test}>
          Send test
        </button>
        <button type="button" onClick={load}>
          {deliveries ? 'Refresh deliveries' : 'Show deliveries'}
        </button>
        <button type="button" onClick={remove}>
          Delete
        </button>
      </span>
      {note && <span role="status">{note}</span>}
      {deliveries &&
        (deliveries.length === 0 ? (
          <p>No deliveries yet.</p>
        ) : (
          <table className="ledger">
            <thead>
              <tr>
                <th>Created</th>
                <th>Event</th>
                <th>Status</th>
                <th>Attempts</th>
                <th>Last answer</th>
              </tr>
            </thead>
            <tbody>
              {deliveries.map((d) => (
                <tr key={d.id}>
                  <td>{new Date(d.created_at).toLocaleString()}</td>
                  <td>{d.event_type}</td>
                  <td>{d.status}</td>
                  <td>{d.attempts}</td>
                  <td>{d.last_error ?? (d.last_status_code !== null ? `HTTP ${d.last_status_code}` : '')}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ))}
    </li>
  )
}
