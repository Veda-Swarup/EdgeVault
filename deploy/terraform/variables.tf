variable "aws_region" {
  type    = string
  default = "ap-south-1"
}

variable "project_name" {
  type    = string
  default = "edgevault"
}

variable "kubernetes_version" {
  type    = string
  default = "1.37"
}

variable "cluster_admin_cidrs" {
  type        = list(string)
  description = "Public IPv4 CIDRs allowed to reach the EKS Kubernetes API; use your workstation's public IPv4 /32."
}

variable "db_username" {
  type    = string
  default = "edgevault"
}

variable "db_instance_class" {
  type    = string
  default = "db.t4g.micro"
}

variable "node_instance_types" {
  type    = list(string)
  default = ["t3.medium"]
}

variable "snapshot_retention_days" {
  type    = number
  default = 7
}
