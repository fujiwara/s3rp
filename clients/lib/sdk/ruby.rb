# The verbs of defs/sdk-ruby.yml, on the AWS SDK for Ruby.
require "bundler/inline"
require "erb"

gemfile do
  source "https://rubygems.org"
  gem "aws-sdk-s3", "1.245.1"
  # the SDK needs an XML parser and bundles none
  gem "rexml", "3.4.4"
end

env = ENV
bucket = env.fetch("BUCKET")
s3 = Aws::S3::Client.new(
  endpoint: env.fetch("ENDPOINT"),
  region: "us-east-1",
  force_path_style: true,
  access_key_id: env.fetch("ACCESS_KEY"),
  secret_access_key: env.fetch("SECRET_KEY"),
  ssl_ca_bundle: env.fetch("CA_FILE"),
)
# the transfer manager, so a large file goes up in parts
transfer = Aws::S3::TransferManager.new(client: s3)

verb, *args = ARGV
case verb
when "put"
  transfer.upload_file(args[0], bucket: bucket, key: args[1])
when "get"
  transfer.download_file(args[1], bucket: bucket, key: args[0])
when "list"
  s3.list_objects_v2(bucket: bucket, prefix: "#{args[0]}/").each do |page|
    page.contents.each { |object| puts object.key }
  end
when "delete"
  s3.delete_object(bucket: bucket, key: args[0])
when "copy"
  source = args[0].split("/", -1).map { |s| ERB::Util.url_encode(s) }.join("/")
  s3.copy_object(bucket: bucket, key: args[1], copy_source: "#{bucket}/#{source}")
when "presign"
  puts Aws::S3::Presigner.new(client: s3).presigned_url(:get_object, bucket: bucket, key: args[0], expires_in: 3600)
when "put_tree"
  Dir.children(args[0]).sort.each do |name|
    transfer.upload_file(File.join(args[0], name), bucket: bucket, key: "#{args[1]}/#{name}")
  end
else
  abort "unknown verb #{verb}"
end
