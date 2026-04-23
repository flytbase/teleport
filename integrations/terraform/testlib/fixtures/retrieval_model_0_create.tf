resource "teleport_retrieval_model" "test" {
  version = "v1"

  spec = {
    bedrock = {
      region           = "us-west-2"
      bedrock_model_id = "amazon.titan-embed-text-v2:0"
    }
    inference_model_name = "bedrock-model"
  }
}
