const assert = require("node:assert");
const greet = require("sbx-greeting");

assert.strictEqual(greet("sbx"), "hello, sbx");
console.log("ok:", greet("sbx"));
