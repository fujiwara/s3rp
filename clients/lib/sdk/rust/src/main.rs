// The verbs of defs/sdk-rust.yml, on the AWS SDK for Rust.
use std::env;
use std::error::Error;
use std::path::Path;
use std::time::Duration;

use aws_config::{BehaviorVersion, Region};
use aws_sdk_s3::config::Credentials;
use aws_sdk_s3::presigning::PresigningConfig;
use aws_sdk_s3::primitives::{ByteStream, Length};
use aws_sdk_s3::types::{CompletedMultipartUpload, CompletedPart};
use aws_sdk_s3::Client;
use tokio::io::AsyncWriteExt;

const PART_SIZE: u64 = 16 * 1024 * 1024;

type Result<T> = std::result::Result<T, Box<dyn Error>>;

// The SDK has no transfer manager of its own, so a large file goes up in
// parts the way its documentation shows.
async fn upload(s3: &Client, bucket: &str, file: &str, key: &str) -> Result<()> {
    let size = tokio::fs::metadata(file).await?.len();
    if size <= PART_SIZE {
        let body = ByteStream::from_path(Path::new(file)).await?;
        s3.put_object().bucket(bucket).key(key).body(body).send().await?;
        return Ok(());
    }
    let created = s3.create_multipart_upload().bucket(bucket).key(key).send().await?;
    let upload_id = created.upload_id().ok_or("no upload id")?;
    let mut parts = Vec::new();
    let mut offset = 0;
    while offset < size {
        let number = parts.len() as i32 + 1;
        let body = ByteStream::read_from()
            .path(file)
            .offset(offset)
            .length(Length::Exact(PART_SIZE.min(size - offset)))
            .build()
            .await?;
        let part = s3
            .upload_part()
            .bucket(bucket)
            .key(key)
            .upload_id(upload_id)
            .part_number(number)
            .body(body)
            .send()
            .await?;
        parts.push(
            CompletedPart::builder()
                .part_number(number)
                .set_e_tag(part.e_tag().map(str::to_owned))
                .build(),
        );
        offset += PART_SIZE;
    }
    s3.complete_multipart_upload()
        .bucket(bucket)
        .key(key)
        .upload_id(upload_id)
        .multipart_upload(CompletedMultipartUpload::builder().set_parts(Some(parts)).build())
        .send()
        .await?;
    Ok(())
}

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<String> = env::args().skip(1).collect();
    let bucket = env::var("BUCKET")?;
    let config = aws_config::defaults(BehaviorVersion::latest())
        .region(Region::new("us-east-1"))
        .endpoint_url(env::var("ENDPOINT")?)
        .credentials_provider(Credentials::new(
            env::var("ACCESS_KEY")?,
            env::var("SECRET_KEY")?,
            None,
            None,
            "static",
        ))
        .load()
        .await;
    let s3 = Client::from_conf(
        aws_sdk_s3::config::Builder::from(&config).force_path_style(true).build(),
    );

    match args[0].as_str() {
        "put" => upload(&s3, &bucket, &args[1], &args[2]).await?,
        "get" => {
            let mut object = s3.get_object().bucket(&bucket).key(&args[1]).send().await?;
            let mut out = tokio::fs::File::create(&args[2]).await?;
            while let Some(chunk) = object.body.try_next().await? {
                out.write_all(&chunk).await?;
            }
            out.flush().await?;
        }
        "list" => {
            let mut pages = s3
                .list_objects_v2()
                .bucket(&bucket)
                .prefix(format!("{}/", args[1]))
                .into_paginator()
                .send();
            while let Some(page) = pages.next().await {
                for object in page?.contents() {
                    println!("{}", object.key().unwrap_or_default());
                }
            }
        }
        "delete" => {
            s3.delete_object().bucket(&bucket).key(&args[1]).send().await?;
        }
        "copy" => {
            s3.copy_object()
                .bucket(&bucket)
                .key(&args[2])
                .copy_source(format!("{}/{}", bucket, args[1]))
                .send()
                .await?;
        }
        "presign" => {
            let request = s3
                .get_object()
                .bucket(&bucket)
                .key(&args[1])
                .presigned(PresigningConfig::expires_in(Duration::from_secs(3600))?)
                .await?;
            println!("{}", request.uri());
        }
        "put_tree" => {
            let mut names = Vec::new();
            let mut entries = tokio::fs::read_dir(&args[1]).await?;
            while let Some(entry) = entries.next_entry().await? {
                names.push(entry.file_name().to_string_lossy().into_owned());
            }
            names.sort();
            for name in names {
                upload(&s3, &bucket, &format!("{}/{}", args[1], name), &format!("{}/{}", args[2], name)).await?;
            }
        }
        verb => return Err(format!("unknown verb {verb}").into()),
    }
    Ok(())
}
