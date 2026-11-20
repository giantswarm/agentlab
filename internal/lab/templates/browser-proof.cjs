const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const prefix = '/opt/kagent/tools/node';
process.env.FONTCONFIG_FILE = prefix + '/etc/fonts/fonts.conf';
process.env.ICU_DATA ||= process.env.NODE_ICU_DATA;
const packages = prefix + '/usr/lib/node_modules';
const { chromium } = require(packages + '/playwright');
const target = new URL(process.argv[2]);
const marker = process.argv[3];
const memoryLimit = Number(process.argv[4]);
const report = (key, value) => console.log(`${marker} browser-${key}=${value}`);
const payload = JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {
  protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'agentlab-browser-proof', version: '1' },
} });
const placeholder = 'Bearer kagent-credential-placeholder';
const wrapper = prefix + '/bin/chromium';
const raw = prefix + '/usr/lib/chromium/chromium';
const launch = executablePath => chromium.launch({ executablePath, headless: true, args: [
  '--no-sandbox', '--disable-gpu', '--disable-background-networking', '--disable-component-update',
  '--disable-sync', '--no-first-run',
] });

function observe(context, destination) {
  const readings = { responses: [], failures: [], continued: 0 };
  const matches = request => request.url().startsWith(destination);
  context.on('response', response => {
    if (matches(response.request())) readings.responses.push({ method: response.request().method(), status: response.status() });
  });
  context.on('requestfailed', request => {
    if (matches(request)) readings.failures.push(request.failure().errorText);
  });
  return readings;
}

async function fetchFromPage(page, destination, options) {
  return page.evaluate(async ({ destination, options }) => {
    try {
      const response = await fetch(destination, { ...options, signal: AbortSignal.timeout(10000) });
      return response.type;
    } catch (error) {
      return error.name;
    }
  }, { destination, options });
}

async function noRoute(origin) {
  const browser = await launch(raw);
  try {
    for (const [name, options, headers, status] of [
      ['preflight', { method: 'POST', headers: { Authorization: placeholder }, body: payload }, {}, null],
      ['anonymous', { method: 'POST', mode: 'no-cors', body: payload }, {}, 401],
      ['extra-header', { method: 'POST', mode: 'no-cors', body: payload }, { Authorization: placeholder }, 400],
    ]) {
      const context = await browser.newContext({ extraHTTPHeaders: headers, serviceWorkers: 'block' });
      try {
        const readings = observe(context, target.href);
        const page = await context.newPage();
        await page.goto(origin);
        const result = await fetchFromPage(page, target.href, options);
        console.log(JSON.stringify({ case: name, result, ...readings }));
        if (status === null) {
          assert.equal(result, 'TypeError');
          assert(readings.failures.includes('net::ERR_FAILED'));
          assert(!readings.responses.some(response => response.method === 'POST'));
          report(name, 'blocked');
        } else {
          assert(readings.responses.some(response => response.method === 'POST' && response.status === status));
          report(name, String(status));
        }
      } finally {
        await context.close();
      }
    }
  } finally {
    await browser.close();
  }
}

async function wrapped(origin, destination, suffix) {
  const browser = await launch(wrapper);
  try {
    for (const name of ['fetch', 'redirect', 'worker']) {
      const context = await browser.newContext({ serviceWorkers: name === 'worker' ? 'allow' : 'block' });
      try {
        const readings = observe(context, destination);
        await context.route('**/*', route => {
          readings.continued++;
          return route.continue();
        });
        const page = await context.newPage();
        await page.goto(origin);
        if (name === 'fetch') {
          await fetchFromPage(page, destination, { method: 'POST', headers: { Authorization: placeholder }, body: payload });
        } else if (name === 'redirect') {
          await page.goto(origin + '/redirect?to=' + encodeURIComponent(destination), { timeout: 15000 }).catch(() => {});
        } else {
          const result = await page.evaluate(async destination => {
            await navigator.serviceWorker.register('/worker.js');
            const registration = await navigator.serviceWorker.ready;
            return new Promise((resolve, reject) => {
              const channel = new MessageChannel();
              const timeout = setTimeout(() => reject(new Error('worker fetch timed out')), 15000);
              channel.port1.onmessage = event => { clearTimeout(timeout); resolve(event.data); };
              registration.active.postMessage(destination, [channel.port2]);
            });
          }, destination);
          assert.equal(result, 'TypeError');
        }
        console.log(JSON.stringify({ case: name + suffix, ...readings }));
        assert(readings.continued > 0);
        assert(readings.failures.includes('net::ERR_NAME_NOT_RESOLVED'));
        assert.equal(readings.responses.length, 0);
        report(name + suffix, 'net::ERR_NAME_NOT_RESOLVED');
      } finally {
        await context.close();
      }
    }
  } finally {
    await browser.close();
  }
}

async function suite(origin, destination, suffix) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'agentlab-browser-suite-'));
  try {
    fs.writeFileSync(path.join(directory, 'playwright.config.cjs'), `module.exports = {
      testDir: '.', timeout: 30000, workers: 1, reporter: 'line',
      use: { browserName: 'chromium', headless: true, serviceWorkers: 'block',
        launchOptions: { executablePath: ${JSON.stringify(wrapper)}, args: ['--no-sandbox', '--disable-background-networking', '--disable-component-update', '--disable-sync', '--no-first-run'] } }
    };`);
    fs.writeFileSync(path.join(directory, 'browser.spec.cjs'), `
      const { test, expect } = require(${JSON.stringify(packages + '/playwright/test')});
      test('muster refused under route.continue', async ({ page }) => {
        const failures = [];
        const responses = [];
        let continued = 0;
        page.on('requestfailed', request => {
          if (request.url().startsWith(${JSON.stringify(destination)})) failures.push(request.failure().errorText);
        });
        page.on('response', response => {
          if (response.url().startsWith(${JSON.stringify(destination)})) responses.push(response.status());
        });
        await page.route('**/*', route => { continued++; return route.continue(); });
        await page.goto(${JSON.stringify(origin)});
        await page.evaluate(async destination => {
          try { await fetch(destination, { method: 'POST', headers: { Authorization: ${JSON.stringify(placeholder)} }, body: ${JSON.stringify(payload)} }); } catch {}
        }, ${JSON.stringify(destination)});
        expect(continued).toBeGreaterThan(0);
        expect(failures).toContain('net::ERR_NAME_NOT_RESOLVED');
        expect(responses).toEqual([]);
        console.log(${JSON.stringify(marker + ' browser-suite' + suffix + '=net::ERR_NAME_NOT_RESOLVED')});
      });
    `);
    const status = await new Promise((resolve, reject) => {
      const child = spawn(process.execPath, [packages + '/playwright/cli.js', 'test', '-c', directory], {
        cwd: directory, stdio: 'inherit', timeout: 60000, env: { ...process.env, CI: '1' },
      });
      child.on('error', reject);
      child.on('exit', resolve);
    });
    assert.equal(status, 0, 'playwright test must pass');
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
}

async function main() {
  assert(target.hostname.startsWith('muster.'));
  let minimumAvailable = Infinity;
  let maximumTotal = 0;
  const sampleMemory = () => {
    const memory = fs.readFileSync('/proc/meminfo', 'utf8');
    const total = Number(memory.match(/^MemTotal:\s+(\d+)/m)?.[1]) * 1024;
    const available = Number(memory.match(/^MemAvailable:\s+(\d+)/m)?.[1]) * 1024;
    assert(total > 0);
    assert(available > 0 && available <= total);
    minimumAvailable = Math.min(minimumAvailable, available);
    maximumTotal = Math.max(maximumTotal, total);
  };
  sampleMemory();
  const sampler = setInterval(sampleMemory, 50);
  const server = http.createServer((request, response) => {
    const requestURL = new URL(request.url, 'http://localhost');
    if (requestURL.pathname === '/redirect') {
      response.writeHead(302, { Location: requestURL.searchParams.get('to') });
      response.end();
    } else if (requestURL.pathname === '/worker.js') {
      response.writeHead(200, { 'Content-Type': 'application/javascript' });
      response.end(`self.addEventListener('install', event => event.waitUntil(self.skipWaiting()));
        self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));
        self.addEventListener('message', event => event.waitUntil(
          fetch(event.data, { method: 'POST', mode: 'no-cors', body: 'invalid' })
            .then(() => event.ports[0].postMessage('reached'), error => event.ports[0].postMessage(error.name))));`);
    } else {
      response.writeHead(200, { 'Content-Type': 'text/html' });
      response.end('<title>Agentlab browser protocol proof</title><h1>Browser proof</h1>');
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://localhost:${server.address().port}`;
  try {
    await noRoute(origin);
    for (const [destination, suffix] of [[target.href, ''], [target.href.replace(target.hostname, target.hostname + '.'), '-dot']]) {
      await wrapped(origin, destination, suffix);
      await suite(origin, destination, suffix);
    }
    sampleMemory();
    console.log(JSON.stringify({ memoryLimit, minimumAvailable, maximumTotal }));
    report('memory', memoryLimit <= 0 ? 'missing-limit' : maximumTotal <= memoryLimit ? 'bounded' : 'exceeds-limit');
  } finally {
    clearInterval(sampler);
    await new Promise(resolve => server.close(resolve));
  }
}

main().catch(error => { console.error(error); process.exitCode = 1; });
