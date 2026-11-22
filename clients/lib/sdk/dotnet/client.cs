#:package AWSSDK.S3@4.1.2

// The verbs of defs/sdk-dotnet.yml, on the AWS SDK for .NET.
using Amazon.Runtime;
using Amazon.S3;
using Amazon.S3.Model;
using Amazon.S3.Transfer;

string Env(string name) => Environment.GetEnvironmentVariable(name)!;
var bucket = Env("BUCKET");
using var s3 = new AmazonS3Client(
    new BasicAWSCredentials(Env("ACCESS_KEY"), Env("SECRET_KEY")),
    new AmazonS3Config { ServiceURL = Env("ENDPOINT"), AuthenticationRegion = "us-east-1", ForcePathStyle = true });
// the transfer utility, so a large file goes up in parts
using var transfer = new TransferUtility(s3);

switch (args[0])
{
    case "put":
        await transfer.UploadAsync(args[1], bucket, args[2]);
        break;
    case "get":
        await transfer.DownloadAsync(args[2], bucket, args[1]);
        break;
    case "list":
        await foreach (var obj in s3.Paginators.ListObjectsV2(new ListObjectsV2Request { BucketName = bucket, Prefix = args[1] + "/" }).S3Objects)
            Console.WriteLine(obj.Key);
        break;
    case "delete":
        await s3.DeleteObjectAsync(bucket, args[1]);
        break;
    case "copy":
        await s3.CopyObjectAsync(bucket, args[1], bucket, args[2]);
        break;
    case "presign":
        Console.WriteLine(await s3.GetPreSignedURLAsync(new GetPreSignedUrlRequest
        {
            BucketName = bucket, Key = args[1], Verb = HttpVerb.GET, Expires = DateTime.UtcNow.AddHours(1),
            Protocol = Env("SCHEME") == "https" ? Protocol.HTTPS : Protocol.HTTP,
        }));
        break;
    case "lifecycle":
        foreach (var rule in (await s3.GetLifecycleConfigurationAsync(bucket)).Configuration.Rules)
        {
            var prefix = (rule.Filter?.LifecycleFilterPredicate as LifecyclePrefixPredicate)?.Prefix;
            Console.WriteLine($"{rule.Id} prefix={prefix} expire={rule.Expiration?.Days} abort={rule.AbortIncompleteMultipartUpload?.DaysAfterInitiation}");
        }
        break;
    case "replication":
        foreach (var rule in (await s3.GetBucketReplicationAsync(new GetBucketReplicationRequest { BucketName = bucket })).Configuration.Rules)
            Console.WriteLine($"{rule.Id} prefix={rule.Filter?.Prefix} status={rule.Status} destination={rule.Destination?.BucketArn}");
        break;
    case "put_tree":
        foreach (var file in Directory.GetFiles(args[1]).Order())
            await transfer.UploadAsync(file, bucket, args[2] + "/" + Path.GetFileName(file));
        break;
    default:
        throw new ArgumentException($"unknown verb {args[0]}");
}
