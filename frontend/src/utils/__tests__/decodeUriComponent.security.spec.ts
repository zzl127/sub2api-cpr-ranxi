import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { beforeAll, describe, expect, it } from 'vitest'

const require = createRequire(import.meta.url)
const iconsRequire = createRequire(require.resolve('@lobehub/icons'))
const uiRequire = createRequire(iconsRequire.resolve('@lobehub/ui'))
const queryEntry = uiRequire.resolve('query-string')
const decoderEntry = createRequire(queryEntry).resolve('decode-uri-component')
const decoderVersion = JSON.parse(readFileSync(join(dirname(decoderEntry), 'package.json'), 'utf8')).version

interface DecoderResult {
  decoded: string[]
  query: Record<string, string | string[]>
  durationMs: number
  outputLength: number
}

describe('GHSA-qr28-fpjm-wq7f transitive URL decoder', () => {
  let result: DecoderResult

  beforeAll(() => {
    expect(decoderVersion).toBe('0.5.1')
    // Use the actual consumer's dependency resolution, not a hoisted/direct test copy.
    // A child-process deadline bounds a future regression that blocks the event loop.
    const script = `
      import { performance } from 'node:perf_hooks';
      const { default: decode } = await import(process.argv[1]);
      const { default: queryString } = await import(process.argv[2]);
      const decoded = [
        'hello%20world', '%E4%BD%A0%E5%A5%BD', '%25C2%zz', '%25%41%zz', '%41%FE%FF', '%C2'
      ].map(value => decode(value));
      const query = queryString.parse('q=hello+world&path=%252F&bad=%C2&tag=a&tag=b');
      const runs = Array.from({ length: 32000 }, (_, index) =>
        '%' + Math.floor(index / 256).toString(16).padStart(2, '0') +
        '%' + (index % 256).toString(16).padStart(2, '0') + 'x');
      const input = runs.join('') + '%zz';
      const start = performance.now();
      const outputLength = decode(input).length;
      process.stdout.write(JSON.stringify({ decoded, query, durationMs: performance.now() - start, outputLength }));
    `
    result = JSON.parse(execFileSync(process.execPath, [
      '--input-type=module', '-e', script,
      pathToFileURL(decoderEntry).href, pathToFileURL(queryEntry).href
    ], { encoding: 'utf8', timeout: 10000, maxBuffer: 16384 }))
  }, 15000)

  it('resolves the patched decoder through the production dependency chain', () => {
    expect(decoderVersion).toBe('0.5.1')
  })

  it('preserves valid decoding and handles malformed input in one pass', () => {
    expect(result.decoded).toEqual([
      'hello world', '\u4f60\u597d', '%C2%zz', '%A%zz', 'A\uFFFD\uFFFD', '\uFFFD'
    ])
    expect(result.query).toEqual({
      q: 'hello world', path: '%2F', bad: '\uFFFD', tag: ['a', 'b']
    })
  })

  it('finishes many distinct encoded runs within a bounded budget', () => {
    expect(result.outputLength).toBeGreaterThan(0)
    expect(result.durationMs).toBeLessThan(5000)
  })
})
