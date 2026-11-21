//JAVA 25
//DEPS software.amazon.awssdk:s3:2.59.7
//DEPS software.amazon.awssdk:s3-transfer-manager:2.59.7
//DEPS org.slf4j:slf4j-nop:1.7.36

// The verbs of defs/sdk-java.yml, on the AWS SDK for Java v2.
import java.io.FileInputStream;
import java.net.URI;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyStore;
import java.security.cert.Certificate;
import java.security.cert.CertificateFactory;
import java.time.Duration;
import java.util.List;
import javax.net.ssl.TrustManagerFactory;
import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.StaticCredentialsProvider;
import software.amazon.awssdk.http.nio.netty.NettyNioAsyncHttpClient;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.S3AsyncClient;
import software.amazon.awssdk.services.s3.S3Configuration;
import software.amazon.awssdk.services.s3.presigner.S3Presigner;
import software.amazon.awssdk.transfer.s3.S3TransferManager;

public class Client {
    public static void main(String[] args) throws Exception {
        var env = System.getenv();
        var bucket = env.get("BUCKET");
        var endpoint = URI.create(env.get("ENDPOINT"));
        var credentials = StaticCredentialsProvider.create(
            AwsBasicCredentials.create(env.get("ACCESS_KEY"), env.get("SECRET_KEY")));

        // the run's CA is its own, and the JVM reads no CA
        // bundle from the environment
        var trust = KeyStore.getInstance(KeyStore.getDefaultType());
        trust.load(null, null);
        try (var in = new FileInputStream(env.get("CA_FILE"))) {
            int i = 0;
            for (Certificate c : CertificateFactory.getInstance("X.509").generateCertificates(in)) {
                trust.setCertificateEntry("ca" + i++, c);
            }
        }
        var factory = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm());
        factory.init(trust);

        try (var http = NettyNioAsyncHttpClient.builder().tlsTrustManagersProvider(factory::getTrustManagers).build();
             // multipart, so the transfer manager sends a large file in parts
             var s3 = S3AsyncClient.builder()
                 .endpointOverride(endpoint).region(Region.US_EAST_1).forcePathStyle(true)
                 .credentialsProvider(credentials).httpClient(http).multipartEnabled(true).build();
             var transfer = S3TransferManager.builder().s3Client(s3).build()) {
            switch (args[0]) {
                case "put" -> upload(transfer, bucket, args[1], args[2]);
                case "get" -> {
                    Files.deleteIfExists(Path.of(args[2]));
                    transfer.downloadFile(d -> d.getObjectRequest(g -> g.bucket(bucket).key(args[1]))
                        .destination(Path.of(args[2]))).completionFuture().join();
                }
                case "list" -> s3.listObjectsV2Paginator(l -> l.bucket(bucket).prefix(args[1] + "/"))
                    .contents().subscribe(o -> System.out.println(o.key())).join();
                case "delete" -> s3.deleteObject(d -> d.bucket(bucket).key(args[1])).join();
                case "copy" -> s3.copyObject(c -> c.sourceBucket(bucket).sourceKey(args[1])
                    .destinationBucket(bucket).destinationKey(args[2])).join();
                case "presign" -> {
                    try (var presigner = S3Presigner.builder()
                            .endpointOverride(endpoint).region(Region.US_EAST_1).credentialsProvider(credentials)
                            .serviceConfiguration(S3Configuration.builder().pathStyleAccessEnabled(true).build())
                            .build()) {
                        System.out.println(presigner.presignGetObject(p -> p
                            .signatureDuration(Duration.ofHours(1))
                            .getObjectRequest(g -> g.bucket(bucket).key(args[1]))).url());
                    }
                }
                case "put_tree" -> {
                    List<Path> files;
                    try (var entries = Files.list(Path.of(args[1]))) {
                        files = entries.sorted().toList();
                    }
                    for (Path file : files) {
                        upload(transfer, bucket, file.toString(), args[2] + "/" + file.getFileName());
                    }
                }
                default -> throw new IllegalArgumentException("unknown verb " + args[0]);
            }
        }
    }

    static void upload(S3TransferManager transfer, String bucket, String file, String key) {
        transfer.uploadFile(u -> u.source(Path.of(file)).putObjectRequest(p -> p.bucket(bucket).key(key)))
            .completionFuture().join();
    }
}
