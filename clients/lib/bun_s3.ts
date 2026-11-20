// The verbs of defs/bun.yml, on Bun's built-in S3 client.
import { S3Client } from "bun";
import { readdirSync } from "node:fs";

const env = process.env as Record<string, string>;
const s3 = new S3Client({
  endpoint: env.ENDPOINT,
  bucket: env.BUCKET,
  accessKeyId: env.ACCESS_KEY,
  secretAccessKey: env.SECRET_KEY,
  region: "us-east-1",
});

const [verb, ...args] = process.argv.slice(2);
switch (verb) {
  case "put":
    await s3.write(args[1], Bun.file(args[0]));
    break;
  case "get":
    await Bun.write(args[1], s3.file(args[0]));
    break;
  case "list": {
    let startAfter: string | undefined;
    for (;;) {
      const page = await s3.list({ prefix: args[0] + "/", startAfter });
      for (const object of page.contents ?? []) console.log(object.key);
      if (!page.isTruncated) break;
      startAfter = page.contents!.at(-1)!.key;
    }
    break;
  }
  case "delete":
    await s3.delete(args[0]);
    break;
  case "presign":
    console.log(s3.presign(args[0], { expiresIn: 3600 }));
    break;
  case "put_tree":
    for (const name of readdirSync(args[0]).sort())
      await s3.write(`${args[1]}/${name}`, Bun.file(`${args[0]}/${name}`));
    break;
  default:
    throw new Error(`unknown verb ${verb}`);
}
