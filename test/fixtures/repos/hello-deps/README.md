# hello-deps

A Node.js project with one dependency, vendored so `npm ci` works without a
registry. Used by the sbx end-to-end tests and demo.

`postinstall` sleeps for 5 seconds to stand in for a slow install, so tests
can see commands wait for it. A cache hit skips it.
