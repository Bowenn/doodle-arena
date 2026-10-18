import { describe, expect, it } from 'vitest'
import { ApiError } from './api.ts'
import { IdempotencyKeys, keepsKey } from './idempotency.ts'

function keys() {
  let n = 0
  return new IdempotencyKeys(() => `k${++n}`)
}

describe('IdempotencyKeys', () => {
  it('gives each new action a new key once the last one finished', async () => {
    const k = keys()
    const seen: string[] = []
    await k.run('buy:l1', async (key) => seen.push(key))
    await k.run('buy:l1', async (key) => seen.push(key))
    expect(seen).toEqual(['k1', 'k2'])
  })

  it('reuses the key when a retry follows a network error, a 409 in progress or a 5xx', async () => {
    const k = keys()
    const seen: string[] = []
    const failures = [
      new TypeError('Failed to fetch'),
      new ApiError('IDEMPOTENCY_KEY_IN_PROGRESS', 409, 'in progress'),
      new ApiError('INTERNAL', 500, 'boom'),
    ]
    for (const failure of failures) {
      await expect(
        k.run('buy:l1', async (key) => {
          seen.push(key)
          throw failure
        }),
      ).rejects.toBe(failure)
    }
    await k.run('buy:l1', async (key) => seen.push(key))
    expect(seen).toEqual(['k1', 'k1', 'k1', 'k1'])
  })

  it('drops the key after a final answer such as 409 sold or 422 insufficient funds', async () => {
    const k = keys()
    const seen: string[] = []
    await expect(
      k.run('buy:l1', async (key) => {
        seen.push(key)
        throw new ApiError('INSUFFICIENT_FUNDS', 422, 'not enough coins')
      }),
    ).rejects.toThrow()
    await k.run('buy:l1', async (key) => seen.push(key))
    expect(seen).toEqual(['k1', 'k2'])
  })

  it('keeps separate keys for separate actions', async () => {
    const k = keys()
    const seen: string[] = []
    await expect(
      k.run('buy:l1', async (key) => {
        seen.push(key)
        throw new TypeError('offline')
      }),
    ).rejects.toThrow()
    await k.run('buy:l2', async (key) => seen.push(key))
    await k.run('buy:l1', async (key) => seen.push(key))
    expect(seen).toEqual(['k1', 'k2', 'k1'])
  })

  it('treats only unknown outcomes as retryable', () => {
    expect(keepsKey(new TypeError('x'))).toBe(true)
    expect(keepsKey(new ApiError('LISTING_NOT_AVAILABLE', 409, 'sold'))).toBe(false)
    expect(keepsKey(new ApiError('IDEMPOTENCY_KEY_REUSED', 422, 'reused'))).toBe(false)
    expect(keepsKey(new ApiError('HTTP_502', 502, 'bad gateway'))).toBe(true)
  })
})
