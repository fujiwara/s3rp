#!/usr/bin/env node
// Generate SigV4 cross-check vectors signed by aws4 (https://github.com/mhart/aws4),
// the de-facto independent SigV4 signer of the Node ecosystem — many Node
// clients sign with it instead of the AWS SDK for JavaScript, and its
// query canonicalization is its own code, not a port of an SDK's. Every
// request it signs must verify.
//
// Deterministic per seed, like generate.py: the clock is pinned by
// overriding Date before aws4 loads, so the committed corpus regenerates
// identically from the same aws4 version.
//
//     NODE_PATH=<dir with aws4 installed> node generate_aws4.js --seed 20260815 --count 96 --out aws4.json

'use strict'

const FIXED_MS = Date.UTC(2026, 7, 1, 12, 0, 0)
const RealDate = Date
global.Date = class extends RealDate {
  constructor(...args) {
    if (args.length === 0) super(FIXED_MS)
    else super(...args)
  }
  static now() { return FIXED_MS }
}

const aws4 = require('aws4')
const fs = require('fs')

// The shape sets of generate.py, restricted to what aws4 itself
// canonicalizes compatibly. Unlike every other signer here, aws4 does not
// sign the S3 path as sent: it decodes each segment and re-encodes it to
// the strict RFC 3986 set (so a raw ':', '*', '+', ... in the path signs
// as its escaped form while the wire carries it raw), and it collapses an
// encoded %2F back to a literal slash. Real aws4 clients therefore send
// paths in that canonical encoding — which is what these tokens are — and
// keys containing an encoded slash cannot be signed by aws4 at all.
const PATH_TOKENS = [
  'photos', 'backup-2026', 'a', '0', '~user', '_x', 'file.txt', '...',
  '..', '.', '', '%20', '%E3%81%82', '%00', '%7F', '%25',
  'a%2Ab', 'a%27b', '%281%29', '%21x', '%24y', '%26z', '%3Deq', '%3Ac',
  '%40h', 'a%2Bb', 'trailing.', '-lead',
]
const QUERY_KEYS = ['prefix', 'list-type', 'marker', 'a', 'response-content-type', 'q k', 'empty']
const QUERY_VALUES = ['', '2', 'photos/', 'a/b', 'あ', 'text/plain', 'a+b', '*', '~', '100%', 'a b']
const HEADER_NAMES = ['x-amz-meta-note', 'X-Amz-Meta-Tag', 'Content-Type', 'Cache-Control', 'x-amz-meta-empty']
const HEADER_VALUES = ['hello world', 'a  b   c', ' padded ', 'text/plain; charset=utf-8', '', 'no-cache, no-store', '=?utf-8?B?44GC?=']
const REGIONS = ['us-east-1', 'ap-northeast-1', 'eu-central-1', 'moon-crater-7']
const METHODS = ['GET', 'PUT', 'POST', 'DELETE', 'HEAD']
const HOSTS = ['s3.example.com', 's3.example.com:8080', 'storage.internal']

function mulberry32(a) {
  return function () {
    a |= 0; a = (a + 0x6D2B79F5) | 0
    let t = Math.imul(a ^ (a >>> 15), 1 | a)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}
const choice = (rng, arr) => arr[Math.floor(rng() * arr.length)]
const randint = (rng, lo, hi) => lo + Math.floor(rng() * (hi - lo + 1))
function sample(rng, arr, n) {
  const pool = arr.slice(), out = []
  for (let i = 0; i < n; i++) out.push(pool.splice(Math.floor(rng() * pool.length), 1)[0])
  return out
}

// percent-encode like python's quote(safe='-_.~'): the canonical SigV4 set
const enc = s => encodeURIComponent(s).replace(/[!'()*]/g, c => '%' + c.charCodeAt(0).toString(16).toUpperCase())

function makePath(rng) {
  const n = randint(rng, 1, 4), parts = []
  for (let i = 0; i < n; i++) parts.push(choice(rng, PATH_TOKENS))
  return '/' + parts.join('/')
}

function makeQuery(rng) {
  const parts = []
  for (const k of sample(rng, QUERY_KEYS, randint(rng, 0, 3))) {
    const v = choice(rng, QUERY_VALUES)
    parts.push(v || rng() < 0.7 ? `${enc(k)}=${enc(v)}` : enc(k))
  }
  // no repeated pair, unlike generate.py: aws4's S3 mode signs only the
  // first value of a duplicated key while sending every pair
  return parts.join('&')
}

function makeCredentials(rng) {
  const pick = (chars, n) => Array.from({ length: n }, () => chars[Math.floor(rng() * chars.length)]).join('')
  const upperDigits = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789'
  const letters = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ'
  const creds = {
    accessKeyId: 'AKID' + pick(upperDigits, 16),
    secretAccessKey: pick(letters + '0123456789+/', 40),
  }
  if (rng() < 0.33) creds.sessionToken = pick(letters + '0123456789+/=', randint(rng, 16, 200))
  return creds
}

function signVector(rng, i) {
  const method = choice(rng, METHODS)
  const host = choice(rng, HOSTS)
  const query = makeQuery(rng)
  const region = choice(rng, REGIONS)
  const creds = makeCredentials(rng)
  const presigned = rng() < 0.5

  const headers = {}
  const nHeaders = randint(rng, 0, 3)
  for (let j = 0; j < nHeaders; j++) headers[choice(rng, HEADER_NAMES)] = choice(rng, HEADER_VALUES)

  let path = makePath(rng)
  if (presigned) {
    // aws4 signs X-Amz-Expires only if the caller puts it in the query
    const expires = `X-Amz-Expires=${randint(rng, 1, 604800)}`
    path += '?' + (query ? query + '&' : '') + expires
  } else if (query) {
    path += '?' + query
  }

  const opts = { host, path, method, service: 's3', region, headers, body: '', signQuery: presigned }
  aws4.sign(opts, creds)

  return {
    name: `${String(i).padStart(3, '0')}-${presigned ? 'presigned' : 'header'}-${method}`,
    type: presigned ? 'presigned' : 'header',
    method,
    host,
    uri: opts.path, // signQuery rewrites it with the auth params
    headers: Object.entries(opts.headers).filter(([k]) => k.toLowerCase() !== 'host'),
    access_key_id: creds.accessKeyId,
    secret_access_key: creds.secretAccessKey,
    session_token: creds.sessionToken || '',
    region,
    timestamp: '2026-08-01T12:00:00Z',
    payload_hash: opts.headers['X-Amz-Content-Sha256'] || 'UNSIGNED-PAYLOAD',
  }
}

function main() {
  const argv = process.argv.slice(2)
  const flag = (name, dflt) => {
    const i = argv.indexOf('--' + name)
    return i >= 0 ? argv[i + 1] : dflt
  }
  const seed = parseInt(flag('seed', '20260815'), 10)
  const count = parseInt(flag('count', '96'), 10)
  const out = flag('out', 'aws4.json')

  const rng = mulberry32(seed)
  const vectors = []
  for (let i = 0; i < count; i++) vectors.push(signVector(rng, i))
  const version = require('aws4/package.json').version
  const doc = {
    generator: 'sigv4/testdata/crosscheck/generate_aws4.js',
    aws4_version: version,
    seed,
    vectors,
  }
  fs.writeFileSync(out, JSON.stringify(doc, null, 1).replace(/[\u0080-\uffff]/g, c =>
    '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0')) + '\n')
  console.log(`${vectors.length} vectors -> ${out} (aws4 ${version})`)
}

main()
