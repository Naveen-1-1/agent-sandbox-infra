const http = require("node:http");
const greet = require("sbx-greeting");

const port = Number(process.env.PORT || 3000);
http.createServer((_, res) => res.end(greet("visitor") + "\n")).listen(port, () => {
  console.log(`listening on ${port}`);
});
