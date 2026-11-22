// The verbs of defs/sdk-js.yml, on the AWS SDK for JavaScript v3.
const { createReadStream, createWriteStream, readdirSync } = require("node:fs");
const { pipeline } = require("node:stream/promises");
const {
  S3Client, GetObjectCommand, DeleteObjectCommand, CopyObjectCommand, paginateListObjectsV2,
  GetBucketLifecycleConfigurationCommand, GetBucketReplicationCommand,
  GetBucketTaggingCommand, GetBucketNotificationConfigurationCommand,
} = require("@aws-sdk/client-s3");
const { Upload } = require("@aws-sdk/lib-storage");
const { getSignedUrl } = require("@aws-sdk/s3-request-presigner");

const env = process.env;
const Bucket = env.BUCKET;
const s3 = new S3Client({
  endpoint: env.ENDPOINT,
  region: "us-east-1",
  forcePathStyle: true,
  credentials: { accessKeyId: env.ACCESS_KEY, secretAccessKey: env.SECRET_KEY },
});

// lib-storage, so a large file goes up in parts
const upload = (file, Key) => new Upload({ client: s3, params: { Bucket, Key, Body: createReadStream(file) } }).done();

async function main([verb, ...args]) {
  switch (verb) {
    case "put":
      await upload(args[0], args[1]);
      break;
    case "get": {
      const res = await s3.send(new GetObjectCommand({ Bucket, Key: args[0] }));
      await pipeline(res.Body, createWriteStream(args[1]));
      break;
    }
    case "list":
      for await (const page of paginateListObjectsV2({ client: s3 }, { Bucket, Prefix: args[0] + "/" }))
        for (const object of page.Contents ?? []) console.log(object.Key);
      break;
    case "delete":
      await s3.send(new DeleteObjectCommand({ Bucket, Key: args[0] }));
      break;
    case "copy": {
      const source = args[0].split("/").map(encodeURIComponent).join("/");
      await s3.send(new CopyObjectCommand({ Bucket, Key: args[1], CopySource: `${Bucket}/${source}` }));
      break;
    }
    case "presign":
      console.log(await getSignedUrl(s3, new GetObjectCommand({ Bucket, Key: args[0] }), { expiresIn: 3600 }));
      break;
    case "lifecycle": {
      const { Rules } = await s3.send(new GetBucketLifecycleConfigurationCommand({ Bucket }));
      for (const rule of Rules ?? []) {
        const expire = rule.Expiration?.Days ?? "-";
        const abort = rule.AbortIncompleteMultipartUpload?.DaysAfterInitiation ?? "-";
        console.log(`${rule.ID} prefix=${rule.Filter?.Prefix ?? ""} expire=${expire} abort=${abort}`);
      }
      break;
    }
    case "replication": {
      const { ReplicationConfiguration } = await s3.send(new GetBucketReplicationCommand({ Bucket }));
      for (const rule of ReplicationConfiguration?.Rules ?? []) {
        console.log(`${rule.ID} prefix=${rule.Filter?.Prefix ?? ""} status=${rule.Status} destination=${rule.Destination?.Bucket}`);
      }
      break;
    }
    case "tags": {
      const { TagSet } = await s3.send(new GetBucketTaggingCommand({ Bucket }));
      for (const tag of TagSet ?? []) console.log(`${tag.Key}=${tag.Value}`);
      break;
    }
    case "notification": {
      const { TopicConfigurations } = await s3.send(new GetBucketNotificationConfigurationCommand({ Bucket }));
      for (const topic of TopicConfigurations ?? []) {
        console.log(`${topic.Id} topic=${topic.TopicArn} events=${(topic.Events ?? []).join(",")}`);
      }
      break;
    }
    case "put_tree":
      for (const name of readdirSync(args[0]).sort()) await upload(`${args[0]}/${name}`, `${args[1]}/${name}`);
      break;
    default:
      throw new Error(`unknown verb ${verb}`);
  }
}

main(process.argv.slice(2)).catch((err) => {
  console.error(err);
  process.exit(1);
});
