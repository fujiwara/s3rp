<?php
// The verbs of defs/sdk-php.yml, on the AWS SDK for PHP.
require getenv('VENDOR') . '/autoload.php';

use Aws\S3\ObjectUploader;
use Aws\S3\S3Client;

$bucket = getenv('BUCKET');
$s3 = new S3Client([
    'endpoint' => getenv('ENDPOINT'),
    'region' => 'us-east-1',
    'use_path_style_endpoint' => true,
    'credentials' => ['key' => getenv('ACCESS_KEY'), 'secret' => getenv('SECRET_KEY')],
    'http' => ['verify' => getenv('CA_FILE')],
]);

// ObjectUploader, so a large file goes up in parts
$upload = function (string $file, string $key) use ($s3, $bucket): void {
    $source = fopen($file, 'rb');
    (new ObjectUploader($s3, $bucket, $key, $source))->upload();
    fclose($source);
};

[$verb, $args] = [$argv[1], array_slice($argv, 2)];
switch ($verb) {
    case 'put':
        $upload($args[0], $args[1]);
        break;
    case 'get':
        $s3->getObject(['Bucket' => $bucket, 'Key' => $args[0], 'SaveAs' => $args[1]]);
        break;
    case 'list':
        foreach ($s3->getPaginator('ListObjectsV2', ['Bucket' => $bucket, 'Prefix' => $args[0] . '/']) as $page) {
            foreach ($page['Contents'] ?? [] as $object) {
                echo $object['Key'], "\n";
            }
        }
        break;
    case 'delete':
        $s3->deleteObject(['Bucket' => $bucket, 'Key' => $args[0]]);
        break;
    case 'copy':
        $s3->copy($bucket, $args[0], $bucket, $args[1]);
        break;
    case 'presign':
        $command = $s3->getCommand('GetObject', ['Bucket' => $bucket, 'Key' => $args[0]]);
        echo $s3->createPresignedRequest($command, '+1 hour')->getUri(), "\n";
        break;
    case 'lifecycle':
        foreach ($s3->getBucketLifecycleConfiguration(['Bucket' => $bucket])['Rules'] as $rule) {
            $prefix = $rule['Filter']['Prefix'] ?? '';
            $expire = $rule['Expiration']['Days'] ?? '-';
            $abort = $rule['AbortIncompleteMultipartUpload']['DaysAfterInitiation'] ?? '-';
            echo "{$rule['ID']} prefix={$prefix} expire={$expire} abort={$abort}\n";
        }
        break;
    case 'replication':
        foreach ($s3->getBucketReplication(['Bucket' => $bucket])['ReplicationConfiguration']['Rules'] as $rule) {
            $prefix = $rule['Filter']['Prefix'] ?? '';
            echo "{$rule['ID']} prefix={$prefix} status={$rule['Status']} destination={$rule['Destination']['Bucket']}\n";
        }
        break;
    case 'tags':
        foreach ($s3->getBucketTagging(['Bucket' => $bucket])['TagSet'] as $tag) {
            echo "{$tag['Key']}={$tag['Value']}\n";
        }
        break;
    case 'notification':
        foreach ($s3->getBucketNotificationConfiguration(['Bucket' => $bucket])['TopicConfigurations'] ?? [] as $topic) {
            echo "{$topic['Id']} topic={$topic['TopicArn']} events=", implode(',', $topic['Events']), "\n";
        }
        break;
    case 'put_tree':
        $names = array_values(array_diff(scandir($args[0]), ['.', '..']));
        sort($names);
        foreach ($names as $name) {
            $upload($args[0] . '/' . $name, $args[1] . '/' . $name);
        }
        break;
    default:
        fwrite(STDERR, "unknown verb $verb\n");
        exit(1);
}
