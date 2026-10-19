# tflint's bundled Terraform rules (make lint s=terraform): unused declarations, missing
# version constraints, deprecated syntax.
plugin "terraform" {
  enabled = true
  preset  = "recommended"
}
