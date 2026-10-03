# SPDX-License-Identifier: GPL-3.0-or-later
"""Moto, the AWS emulator, with the gaps the stack falls into filled.

    python test/e2e/moto_server.py <port>

Moto deploys CloudFormation itself, so the end-to-end suite drives the real
template rather than a description of it. Four gaps in it would fail the
stack:

- It has no CloudFormation support for AWS::CertificateManager::Certificate.
  This registers that type, backed by moto's own ACM, so the certificate is a
  real ARN the listener can be checked against.
- It drops a load balancer's Type when CloudFormation creates one, so the
  network load balancer becomes an application one and refuses its TLS and TCP
  listeners. This passes the Type through, and answers CanonicalHostedZoneID,
  which the DNS alias is written against, instead of leaving it unresolved.
- It never reads a stack's Outputs after executing a change set, which is how
  the installer deploys, so every output comes back missing. This reads them
  once the change set has been applied, as CloudFormation does.
- On an update it hands an SSM-typed parameter's previous value back as the
  resolved value (the AMI id) and then looks that up as a parameter name,
  which fails a second workstation joining an existing stack. A value that is
  not a parameter name is taken as already resolved.
"""

import sys
from typing import Any

from moto.acm.models import acm_backends
from moto.cloudformation.models import ChangeSet
from moto.cloudformation.parsing import ResourceMap
from moto.core.common_models import CloudFormationModel
from moto.elbv2.models import FakeLoadBalancer, elbv2_backends
from moto.server import main


class Certificate(CloudFormationModel):
    # Moto files every model under the service its module names.
    __module__ = "moto.acm.models"

    def __init__(self, arn: str):
        self.arn = arn

    @staticmethod
    def cloudformation_type() -> str:
        return "AWS::CertificateManager::Certificate"

    @staticmethod
    def cloudformation_name_type() -> str:
        return ""

    @property
    def physical_resource_id(self) -> str:
        return self.arn

    @classmethod
    def create_from_cloudformation_json(  # type: ignore[misc]
        cls, resource_name: str, cloudformation_json: Any, account_id: str, region_name: str, **kwargs: Any
    ) -> "Certificate":
        properties = cloudformation_json["Properties"]
        arn = acm_backends[account_id][region_name].request_certificate(
            domain_name=properties["DomainName"],
            idempotency_token=None,
            subject_alt_names=properties.get("SubjectAlternativeNames", []),
            tags=[],
        )
        return cls(arn)


def create_load_balancer(
    cls: type, resource_name: str, cloudformation_json: Any, account_id: str, region_name: str, **kwargs: Any
) -> FakeLoadBalancer:
    properties = cloudformation_json["Properties"]
    return elbv2_backends[account_id][region_name].create_load_balancer(
        resource_name,
        properties.get("SecurityGroups"),
        properties.get("Subnets"),
        scheme=properties.get("Scheme", "internet-facing"),
        loadbalancer_type=properties.get("Type"),
    )


FakeLoadBalancer.create_from_cloudformation_json = classmethod(create_load_balancer)  # type: ignore[method-assign]

# The zone AWS gives every network load balancer in us-east-1.
NETWORK_LOAD_BALANCER_ZONE = "Z26RNL4JYFTOTI"
load_balancer_attribute = FakeLoadBalancer.get_cfn_attribute


def get_load_balancer_attribute(self: FakeLoadBalancer, attribute_name: str) -> Any:
    if attribute_name == "CanonicalHostedZoneID":
        return NETWORK_LOAD_BALANCER_ZONE
    return load_balancer_attribute(self, attribute_name)


FakeLoadBalancer.get_cfn_attribute = get_load_balancer_attribute  # type: ignore[method-assign]


apply_change_set = ChangeSet.apply


def apply_and_read_outputs(self: ChangeSet) -> None:
    apply_change_set(self)
    self.stack.template = self.template
    self.stack._parse_template()
    self.stack.output_map = self.stack._create_output_map()


ChangeSet.apply = apply_and_read_outputs  # type: ignore[method-assign]


resolve_ssm_parameter = ResourceMap.parse_ssm_parameter


def parse_ssm_parameter(self: ResourceMap, value: str, value_type: str) -> str:
    if not value.startswith("/"):
        return value
    return resolve_ssm_parameter(self, value, value_type)


ResourceMap.parse_ssm_parameter = parse_ssm_parameter  # type: ignore[method-assign]


if __name__ == "__main__":
    main(["--port", sys.argv[1] if len(sys.argv) > 1 else "5000"])
