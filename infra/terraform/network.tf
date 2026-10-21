# Two public subnets in two zones that offer the node type. Nodes get
# public addresses and reach the internet through the internet gateway, so
# there's no NAT gateway to pay for; S3 traffic stays inside AWS through a
# free gateway endpoint. Nothing listens publicly: there's no load
# balancer, and the gateway and Grafana are reached by port-forward.
data "aws_ec2_instance_type_offerings" "nodes" {
  location_type = "availability-zone"
  filter {
    name   = "instance-type"
    values = [var.instance_type]
  }
}

locals {
  # us-east-1e can't host EKS control planes.
  azs = slice(sort(setsubtract(data.aws_ec2_instance_type_offerings.nodes.locations, ["us-east-1e"])), 0, 2)
}

resource "aws_vpc" "this" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = local.name }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = local.name }
}

resource "aws_subnet" "public" {
  count                   = length(local.azs)
  vpc_id                  = aws_vpc.this.id
  availability_zone       = local.azs[count.index]
  cidr_block              = cidrsubnet(aws_vpc.this.cidr_block, 4, count.index)
  map_public_ip_on_launch = true
  tags                    = { Name = "${local.name}-${local.azs[count.index]}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
  tags = { Name = local.name }
}

resource "aws_route_table_association" "public" {
  count          = length(aws_subnet.public)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.public.id]
  tags              = { Name = "${local.name}-s3" }
}
