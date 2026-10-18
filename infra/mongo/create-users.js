// One-shot job (mongo-users in infra/docker-compose.yml): creates the assets user, or resets its
// password, so it works on an existing volume too. Mongo's own init scripts run only on an empty
// data directory. The user can read and write the assets database and nothing else.
// Passwords come from the environment, never the command line (which `docker inspect` shows).
const password = process.env.MONGO_ASSETS_PASSWORD;
if (!password || !process.env.MONGO_ROOT_PASSWORD) {
  throw new Error("MONGO_ASSETS_PASSWORD and MONGO_ROOT_PASSWORD must be set");
}
// The health check should only pass once the real server listens, but retry anyway: a refused
// connection here would fail the whole `make up`.
let conn;
for (let attempt = 1; ; attempt++) {
  try {
    conn = new Mongo("mongodb://mongo:27017");
    conn.getDB("admin").auth("root", process.env.MONGO_ROOT_PASSWORD);
    break;
  } catch (e) {
    if (attempt >= 30) throw e;
    print(`mongo not ready (${e.message}); retrying`);
    sleep(1000);
  }
}
const assets = conn.getDB("assets");
const roles = [{ role: "readWrite", db: "assets" }];
if (assets.getUser("assets")) {
  assets.updateUser("assets", { pwd: password, roles });
  print("mongo user assets updated");
} else {
  assets.createUser({ user: "assets", pwd: password, roles });
  print("mongo user assets created");
}
