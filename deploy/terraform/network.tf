data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs           = slice(data.aws_availability_zones.available.names, 0, 2)
  public_cidrs  = ["10.40.0.0/20", "10.40.16.0/20"]
  private_cidrs = ["10.40.32.0/20", "10.40.48.0/20"]
}

resource "aws_vpc" "edgevault" {
  cidr_block           = "10.40.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
}

resource "aws_internet_gateway" "edgevault" {
  vpc_id = aws_vpc.edgevault.id
}

resource "aws_subnet" "public" {
  count                   = 2
  vpc_id                  = aws_vpc.edgevault.id
  availability_zone       = local.azs[count.index]
  cidr_block              = local.public_cidrs[count.index]
  map_public_ip_on_launch = true
  tags = {
    "kubernetes.io/role/elb"                    = "1"
    "kubernetes.io/cluster/${var.project_name}" = "shared"
  }
}

resource "aws_subnet" "private" {
  count             = 2
  vpc_id            = aws_vpc.edgevault.id
  availability_zone = local.azs[count.index]
  cidr_block        = local.private_cidrs[count.index]
  tags = {
    "kubernetes.io/role/internal-elb"           = "1"
    "kubernetes.io/cluster/${var.project_name}" = "shared"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.edgevault.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.edgevault.id
  }
}

resource "aws_route_table_association" "public" {
  count          = 2
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# A single NAT gateway keeps this learning environment cheaper than one NAT per AZ.
resource "aws_eip" "nat" {
  domain     = "vpc"
  depends_on = [aws_internet_gateway.edgevault]
}

resource "aws_nat_gateway" "edgevault" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public[0].id
  depends_on    = [aws_route_table_association.public]
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.edgevault.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.edgevault.id
  }
}

resource "aws_route_table_association" "private" {
  count          = 2
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}
