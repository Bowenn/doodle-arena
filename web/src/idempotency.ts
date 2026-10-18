// Idempotency keys for money actions (rules: web.md). One key per user action, generated when the
// action starts and reused when the same action is retried, so the server runs it at most once.

import { useState } from 'react'
import { ApiError } from './api.ts'

/**
 * True when the outcome is unknown and a retry must reuse the key: the request may or may not have
 * reached the server (a network error), the first attempt is still running (409 in progress), or
 * the server failed and released the key (5xx). Any other answer is final.
 */
export function keepsKey(e: unknown): boolean {
  if (!(e instanceof ApiError)) return true
  return e.code === 'IDEMPOTENCY_KEY_IN_PROGRESS' || e.status >= 500
}

/** Per-action key slots, for example "buy:<listing id>". */
export class IdempotencyKeys {
  private readonly keys = new Map<string, string>()
  private readonly newKey: () => string

  constructor(newKey: () => string = () => crypto.randomUUID()) {
    this.newKey = newKey
  }

  /** Runs one attempt of an action with its key; the key is dropped once the outcome is final. */
  async run<T>(action: string, attempt: (key: string) => Promise<T>): Promise<T> {
    let key = this.keys.get(action)
    if (!key) {
      key = this.newKey()
      this.keys.set(action, key)
    }
    try {
      const result = await attempt(key)
      this.keys.delete(action)
      return result
    } catch (e) {
      if (!keepsKey(e)) this.keys.delete(action)
      throw e
    }
  }
}

/** The component's key slots, kept across renders. */
export function useIdempotencyKeys(): IdempotencyKeys {
  const [keys] = useState(() => new IdempotencyKeys())
  return keys
}
